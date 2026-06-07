package executor

import "bytes"

func forEachResponseLine(data []byte, visit func([]byte) bool) {
	for {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			visit(data)
			return
		}
		if !visit(data[:index]) {
			return
		}
		data = data[index+1:]
	}
}
