package bptree

import "encoding/binary"

// Overflow page layout:
//
//	[0]    type tag (pageOverflow)
//	[1:9]  next page id (uint64, 0 = end of chain)
//	[9:13] payload length on THIS page (uint32)
//	[16..] payload chunk (up to pageSize-overflowHeader bytes)
func allocChain(pg Pager, data []byte) int {
	chunk := pg.PageSize() - overflowHeader
	head := 0
	var tail int
	for len(data) > 0 {
		n := len(data)
		if n > chunk {
			n = chunk
		}
		id := pg.Alloc()
		buf := make([]byte, pg.PageSize())
		buf[offType] = pageOverflow
		// next pointer stays 0; fixed up below when another page follows.
		binary.LittleEndian.PutUint32(buf[offLenHint:], uint32(n))
		copy(buf[overflowHeader:], data[:n])
		if head == 0 {
			head = id
		} else {
			tb := pg.Read(tail)
			putU64(tb, offOverflowNext, uint64(id))
			pg.Write(tail, tb)
		}
		pg.Write(id, buf)
		tail = id
		data = data[n:]
	}
	return head
}

// readChain reassembles the exact payload of an overflow chain headed by id.
func readChain(pg Pager, id int) []byte {
	var out []byte
	for id != 0 {
		buf := pg.Read(id)
		if buf[offType] != pageOverflow {
			panic(ErrCorrupt)
		}
		next := int(u64(buf, offOverflowNext))
		n := int(binary.LittleEndian.Uint32(buf[offLenHint:]))
		if n < 0 || n > pg.PageSize()-overflowHeader {
			panic(ErrCorrupt)
		}
		out = append(out, buf[overflowHeader:overflowHeader+n]...)
		id = next
	}
	return out
}
