package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

func isRedisRESPPrefix(prefix byte) bool {
	switch prefix {
	case '*', '$', '+', '-', ':':
		return true
	default:
		return false
	}
}

func (s *Server) handleRedisConnection(conn net.Conn, reader *bufio.Reader) {
	if s == nil || conn == nil {
		return
	}
	if reader == nil {
		reader = bufio.NewReader(conn)
	}

	writer := bufio.NewWriter(conn)
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("redis connection close error: %v", errClose)
		}
	}()

	if !s.managementRoutesEnabled.Load() {
		_ = writeRedisError(writer, "ERR RESP AUTH disabled; use mTLS")
		if errFlush := writer.Flush(); errFlush != nil {
			log.Errorf("redis protocol flush error: %v", errFlush)
		}
		return
	}

	for {
		args, errRead := readRedisCommand(reader)
		if errRead != nil {
			if errRead != io.EOF {
				log.Debugf("redis protocol read error: %v", errRead)
			}
			return
		}
		if len(args) == 0 {
			continue
		}
		cmd := strings.ToUpper(strings.TrimSpace(args[0]))
		if cmd == "AUTH" {
			_ = writeRedisError(writer, "ERR RESP AUTH disabled; use mTLS")
			if errFlush := writer.Flush(); errFlush != nil {
				log.Errorf("redis protocol flush error: %v", errFlush)
			}
			return
		}
		_ = writeRedisError(writer, "NOAUTH Authentication required.")
		if errFlush := writer.Flush(); errFlush != nil {
			log.Errorf("redis protocol flush error: %v", errFlush)
			return
		}
	}
}

func readRedisCommand(reader *bufio.Reader) ([]string, error) {
	if reader == nil {
		return nil, net.ErrClosed
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected RESP array, got %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil || count < 0 {
		return nil, fmt.Errorf("invalid RESP array length %q", line)
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		header, errHeader := reader.ReadString('\n')
		if errHeader != nil {
			return nil, errHeader
		}
		header = strings.TrimSuffix(strings.TrimSuffix(header, "\n"), "\r")
		if !strings.HasPrefix(header, "$") {
			return nil, fmt.Errorf("expected RESP bulk string, got %q", header)
		}
		size, errSize := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "$")))
		if errSize != nil || size < 0 {
			return nil, fmt.Errorf("invalid RESP bulk length %q", header)
		}
		buf := make([]byte, size+2)
		if _, errRead := io.ReadFull(reader, buf); errRead != nil {
			return nil, errRead
		}
		if string(buf[size:]) != "\r\n" {
			return nil, fmt.Errorf("invalid RESP bulk terminator")
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func writeRedisError(writer *bufio.Writer, message string) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, err := writer.WriteString("-" + message + "\r\n")
	return err
}
