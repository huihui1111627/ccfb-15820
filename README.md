# bptree — 面向长相似键的磁盘 B+ 树索引

钟表零件编码的前十几位是品牌、型号、年份，键又长又像，导致普通 B 树一页装不下几个键、
一次点查要读好几次磁盘。本库用 **Go** 实现一个磁盘驻留 B+ 树，核心目标是**让每个页
装下尽量多的键**，并附带一个能观察索引页内部结构的命令行工具 `bptool`。

## 目录结构

```
bptree/
  types.go        常量、页头布局、公共前缀工具
  prefix.go       最短分隔键、键序比较、前缀后继
  pager.go        Pager 接口：MemoryPager 与带 LRU 缓存/空闲链的 FilePager
  overflow.go     溢出页链（超长键/值）
  page_builder.go 页内槽位目录 + 双向生长的紧凑页布局
  leaf.go         叶子页编码/解码/查找：公共前缀 + 增量压缩 + 重启点
  interior.go     中间页编码/解码/二分查找
  tree.go         插入、智能分裂、删除、子树边界、分裂事件
  scan.go         游标、范围扫描、前缀扫描（沿叶子兄弟指针）
  inspect.go      统计、单页/整树观察、全套不变量校验
  lifecycle.go    Len/Sync/Close
  *_test.go       固定随机种子对拍测试与百万级压力测试
cmd/bptool/       观察工具（demo / stats / dump 三个子命令）
```

## 八项压缩与正确性设计

1. **页公共前缀**：每个页头存本页所有（截断后）键的最长公共前缀，页内只存去掉前缀的
   后缀。插入缩短公共前缀的新键时，整页解码、重算前缀、重新编码（`leafEncode` 是唯一
   写叶子布局的地方，天然完成整理）。

2. **中间页最短分隔键**：分隔键不照抄子页的键，而由 `shortestSeparator(leftMax, rightMin)`
   算出能严格区分左右子树（`leftMax < sep <= rightMin`）的最短字节串。长度下界是
   `LCP+1`：长度等于 LCP 的串要么不大于公共前缀、要么大于 right，都无法分隔。

3. **智能分裂**：分裂不固定从正中切。`splitWindow` 在中点附近取一个窄窗口（半径
   `n/16`，夹在 1..4），对窗口内每个候选切点算分隔键长度，**最短者胜，均衡度作平局
   判据**，并要求两半都能成功编码。窗口保持以有序中点为中心，所以任何候选切点都不会
   破坏键序。

4. **变长键值 + 槽位目录 + 碎片整理**：记录从公共前缀之后向上生长，槽位目录（每条
   `offset+length` 共 4 字节）从页尾向下生长，槽位按逻辑键序排列。所有修改都整页
   重编码，删除产生的空间在重编码时立即回收，因此不存在长期墓碑碎片（`Stats` 里的
   `AvgLeafFill`/`FreeSpace` 直接反映可再用空间）。

5. **超长键溢出页**：键长超过页大小的四分之一（`pageSize/4`）时，**完整键**移到溢出
   页链，页内只保留 `key[:pageSize/4]` 这个截断前缀用于路由。查找时先比截断部分，只有
   截断前缀相同才读溢出链，用完整键裁决（见 `leafSearch`）。超长值同样走溢出链。

6. **叶子增量压缩 + 重启点**：每条非重启记录只存“与前一后缀共享的字节数 + 剩余部分”；
   每隔固定 `restartInterval`(=16) 条放一个存完整后缀的重启点。页内查找先在重启点上
   二分，再顺序解码至多 16 条。

7. **前缀优先的比较**：页内查找先比页公共前缀（`compareKeyPrefix`），前缀相同才进入
   槽位比较；底层用 `bytes.Compare`，循环在第一个不同字节即停止，长共享前缀只付差异
   尾部的比较成本。中间页二分通过 `compareKeySuffix` 不拼接前缀直接比较。

8. **扫描沿兄弟指针**：范围/前缀扫描下钻到起始叶子后沿 `next` 兄弟指针走，不再重复
   下盘走树根。跨页时每个叶子独立重算自己的公共前缀，`leafDecode` 用“本页前缀 + 后缀
   (+溢出链)”拼回完整键，前缀在页边界变化也不会串。前缀扫描的上界由
   `prefixSuccessor` 得到（全 `0xFF` 前缀无上界，扫到末尾）。

## 磁盘布局与缓存

- 页 0 是 meta 页：magic、根页 id、叶子数、空闲链头、页大小、高水位页号。
- `FilePager` 用固定容量 LRU 帧缓存（帧带显式 `linked` 标志维护双向链表），脏帧在
  淘汰或 `Sync/Close` 时 `WriteAt` 落盘并 `fsync`。
- 删除的页（含整条溢出链）进空闲链表，`Alloc` 优先复用。

## 观察工具 bptool

```bash
# 构造固定种子的零件编码索引，打印四个观察面板
go run ./cmd/bptool demo -n 200000 -path index.db
# 仅统计：每层页数、最小/平均/最大扇出、公共前缀长度、压缩前后大小
go run ./cmd/bptool stats -path index.db
# 看单页内部：公共前缀、压缩后的增量元组/重启点、分隔键、兄弟指针、占用/空闲
go run ./cmd/bptool dump -path index.db -page 1
```

四个面板对应：

- **每层页数与平均扇出** — `stats` 的 level 表（level 0 = 叶子层）；
- **单页公共前缀与压缩后的键** — `dump` 的 common prefix 与逐条 stored record
  （`restart suffix=...` / `delta shared=N rest=...`）；
- **分裂选中的分隔键** — `demo` 的 split separator sample（含窗口、选中位置、分隔键长度）；
- **压缩前后索引大小** — `stats` 的 logical bytes vs on-disk bytes 及 ratio。

## 测试

测试用固定随机种子生成带长公共前缀的零件编码，混合插入/删除/点查/扫描，并与一个
始终有序的数组 oracle（`refModel`）逐键对拍；每个阶段后跑 `CheckInvariants`：

- 每页键严格有序；
- 页公共前缀等于真实（截断键的）最长公共前缀；
- 中间页每个分隔键严格区分左右子树且分隔键彼此严格有序；
- 叶子兄弟指针双向一致、重启点节拍正确；
- 树高、扇出、页数、索引大小统计自洽。

```bash
go test ./bptree/ -short            # 快速套件（含小页溢出、磁盘持久化、游标）
go test ./bptree -race -short       # 竞态检测
go test ./bptree -run TestMillionKeyStress -v   # 100 万键压力测试
```

百万键实测（4 KiB 页，内存 pager）：树高 3、叶子平均扇出约 84、平均公共前缀约
17 字节、on-disk/logical 比率约 1.0，混合 20 万次删除/点查/抽样扫描后仍与有序数组
完全一致。
