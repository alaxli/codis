// Copyright 2014 Wandoujia Inc. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package parser

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"

	"github.com/juju/errors"
	respcoding "github.com/ngaut/resp"
)

/*
 * redis protocal : Resp protocol
 * http://redis.io/topics/protocol
 */
var (
	NEW_LINE   = []byte("\r\n")
	EMPTY_LINE []byte
)

const (
	ErrorResp = iota
	SimpleString
	IntegerResp
	BulkResp
	MultiResp
	NoKey
)

type Resp struct {
	Type  int
	Raw   []byte
	Multi []*Resp
}

var (
	noKeyOps = map[string]string{
		"PING":       "fakeKey",
		"SLOTSNUM":   "fakeKey",
		"SLOTSCHECK": "fakeKey",
	}

	keyFun    = make(map[string]funGetKeys)
	intBuffer [][]byte
)

func init() {
	for _, v := range thridAsKeyTbl {
		keyFun[v] = thridAsKey
	}

	cnt := 10000
	intBuffer = make([][]byte, cnt)
	for i := 0; i < cnt; i++ {
		intBuffer[i] = []byte(strconv.Itoa(i))
	}
}

func Itoa(i int) []byte {
	if i < 0 {
		return []byte(strconv.Itoa(i))
	}

	if i < len(intBuffer) {
		return intBuffer[i]
	}

	return []byte(strconv.Itoa(i))
}

//todo: overflow
func Btoi(b []byte) (int, error) {
	n := 0
	sign := 1
	for i := uint8(0); i < uint8(len(b)); i++ {
		if i == 0 && b[i] == '-' {
			if len(b) == 1 {
				return 0, errors.Errorf("Invalid number %s", string(b))
			}
			sign = -1
			continue
		}

		if b[i] >= 0 && b[i] <= '9' {
			if i > 0 {
				n *= 10
			}
			n += int(b[i]) - '0'
			continue
		}

		return 0, errors.Errorf("Invalid number %s", string(b))
	}

	return sign * n, nil
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(line) < 2 || line[len(line)-2] != '\r' { // \r\n
		return nil, errors.Errorf("invalid redis packet %v, err:%v", line, err)
	}

	return line, nil
}

func raw2Bulk(r *Resp) []byte {
	return r.Raw[1 : len(r.Raw)-2] //skip type &&  \r\n
}

func raw2Error(r *Resp) []byte {
	return r.Raw[1 : len(r.Raw)-2] //skip type &&  \r\n
}

func (r *Resp) Op() ([]byte, error) {
	if len(r.Multi) > 0 {
		op := raw2Bulk(r.Multi[0])
		startPos := bytes.IndexByte(op, '\n')
		if startPos < 0 {
			return nil, errors.Errorf("invalid resp %+v", r)
		}

		return op[startPos+1:], nil
	}

	return nil, errors.Errorf("invalid resp %+v", r)
}

type funGetKeys func(r *Resp) ([][]byte, error)

func defaultGetKeys(r *Resp) ([][]byte, error) {
	count := len(r.Multi[1:])
	if count == 0 {
		return nil, nil
	}

	keys := make([][]byte, 0, count)
	for _, v := range r.Multi[1:] {
		key := raw2Bulk(v)
		startPos := bytes.IndexByte(key, '\n')
		if startPos < 0 {
			return nil, errors.Errorf("invalid resp %+v", r)
		}
		keys = append(keys, key[startPos+1:])
	}

	return keys, nil
}

func Parse(r *bufio.Reader) (*Resp, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, errors.Trace(err)
	}

	resp := &Resp{Raw: line}

	switch line[0] {
	case '-':
		resp.Type = ErrorResp
		return resp, nil
	case '+':
		resp.Type = SimpleString
		return resp, nil
	case ':':
		resp.Type = IntegerResp
		return resp, nil
	case '$':
		resp.Type = BulkResp
		size, err := Btoi(line[1 : len(line)-2])
		if err != nil {
			return nil, errors.Trace(err)
		}
		err = ReadBulk(r, size, &resp.Raw)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return resp, nil
	case '*':
		i, err := Btoi(line[1 : len(line)-2]) //strip \r\n
		if err != nil {
			return nil, errors.Trace(err)
		}
		resp.Type = MultiResp
		if i >= 0 {
			multi := make([]*Resp, i)
			for j := 0; j < i; j++ {
				rp, err := Parse(r)
				if err != nil {
					return nil, errors.Trace(err)
				}
				multi[j] = rp
			}
			resp.Multi = multi
		}
		return resp, nil
	default:
		if !IsLetter(line[0]) { //handle telnet text command
			return nil, errors.New("redis protocol error, " + string(line))
		}

		resp.Type = MultiResp
		strs := strings.Split(string(line), " ")

		resp.Raw = make([]byte, 0, 20)
		resp.Raw = append(resp.Raw, '*')
		resp.Raw = append(resp.Raw, []byte(strconv.Itoa(len(strs)))...)
		resp.Raw = append(resp.Raw, NEW_LINE...)
		for i := 0; i < len(strs); i++ { //last element is \r\n
			if str := strings.TrimSpace(strs[i]); len(str) > 0 {
				b, err := respcoding.Marshal(str)
				if err != nil {
					return nil, errors.New("redis protocol error, " + string(line))
				}

				resp.Multi = append(resp.Multi, &Resp{Type: BulkResp, Raw: b})
			}
		}
		return resp, nil
	}

	return nil, errors.New("redis protocol error, " + string(line))
}

func IsLetter(c byte) bool {
	if c >= 'a' && c <= 'z' {
		return true
	}

	if c >= 'A' && c <= 'Z' {
		return true
	}

	return false
}

func ReadBulk(r *bufio.Reader, size int, raw *[]byte) error {
	if size < 0 {
		return nil
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	*raw = append(*raw, buf...)

	line, err := readLine(r)
	if err != nil {
		return errors.Trace(err)
	}

	*raw = append(*raw, line...)

	if len(line) != 2 {
		return errors.New("should be just 0 " + string(line))
	}

	return nil
}

var thridAsKeyTbl = []string{"ZINTERSTORE", "ZUNIONSTORE", "EVAL", "EVALSHA"}

func thridAsKey(r *Resp) ([][]byte, error) {
	if len(r.Multi) < 4 { //if EVAL with no key
		return [][]byte{[]byte("fakeKey")}, nil
	}

	numKeys, err := Btoi(raw2Bulk(r.Multi[2]))
	if err != nil {
		return nil, errors.Trace(err)
	}

	var keys [][]byte
	for _, v := range r.Multi[3:] {
		keys = append(keys, raw2Bulk(v))
		if len(keys) == numKeys {
			break
		}
	}

	return keys, nil
}

func (r *Resp) Keys() ([][]byte, error) {
	key, err := r.Op()
	if err != nil {
		return nil, errors.Trace(err)
	}

	f, ok := keyFun[string(key)]
	if !ok {
		return defaultGetKeys(r)
	}

	return f(r)
}

func (r *Resp) Key() ([]byte, error) {
	keys, err := r.Keys()
	if len(keys) > 0 {
		return keys[0], err
	}

	return []byte("fakeKey"), nil
}

func (r *Resp) getBulkBuf() []byte {
	return r.Raw
}

func (r *Resp) getSimpleStringBuf() []byte {
	return r.Raw
}

func (r *Resp) getErrorBuf() []byte {
	return r.Raw
}

func (r *Resp) getIntegerBuf() []byte {
	return r.Raw
}

func (r *Resp) Bytes() ([]byte, error) {
	var buf []byte
	switch r.Type {
	case NoKey:
		buf = append(buf, raw2Bulk(r)...)
		buf = append(buf, NEW_LINE...)
	case SimpleString:
		buf = r.getSimpleStringBuf()
	case ErrorResp:
		buf = r.getErrorBuf()
	case IntegerResp:
		buf = r.getIntegerBuf()
	case BulkResp:
		buf = r.getBulkBuf()
	case MultiResp:
		buf = make([]byte, 0, 256)
		buf = append(buf, r.Raw...)

		if len(r.Multi) > 0 {
			for _, resp := range r.Multi {
				slice, err := resp.Bytes()
				if err != nil {
					return nil, errors.Trace(err)
				}
				buf = append(buf, slice...)
			}
		}
	}

	return buf, nil
}
