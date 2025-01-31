// Copyright 2016 PLUMgrid
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bcc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/vietanhduong/gobpf/pkg/cpupossible"
)

/*
#cgo CFLAGS: -I/usr/include/bcc
#cgo LDFLAGS: -lbcc

#include <bcc/bcc_common.h>
#include <bcc/libbpf.h>
#include <bcc/bcc_syms.h>
#include <linux/bpf.h>
#include <linux/elf.h>

struct stacktrace_t {
  uintptr_t ip[127];
};

*/
import "C"

var errIterationFailed = errors.New("table.Iter: leaf for next key not found")

// Table references a BPF table.  The zero value cannot be used.
type Table struct {
	id        C.size_t
	fd        C.int
	module    *Module
	pidSym    map[int]unsafe.Pointer
	symbolOpt C.struct_bcc_symbol_option
}

type Config struct {
	Name     string
	Fd       int
	KeySize  uint64
	LeafSize uint64
	KeyDesc  string
	LeafDesc string
	Capacity int
}

// New tables returns a refernce to a BPF table.
func NewTable(id C.size_t, module *Module) *Table {
	return &Table{
		id:     id,
		module: module,
		fd:     C.bpf_table_fd_id(module.p, id),
		pidSym: make(map[int]unsafe.Pointer),
		symbolOpt: C.struct_bcc_symbol_option{
			use_debug_file:       1,
			check_debug_file_crc: 1,
			lazy_symbolize:       1,
			use_symbol_type:      (1 << C.STT_FUNC) | (1 << C.STT_GNU_IFUNC),
		},
	}
}

func (table *Table) UpdateSymbolOptions(useDebugFile, checkDebugFileCrc, lazySymbolize bool, useSymbolType int) {
	table.symbolOpt.use_debug_file = C.int(boolToInt(useDebugFile))
	table.symbolOpt.check_debug_file_crc = C.int(boolToInt(checkDebugFileCrc))
	table.symbolOpt.lazy_symbolize = C.int(boolToInt(lazySymbolize))
	table.symbolOpt.use_symbol_type = C.uint(useSymbolType)
}

// ID returns the table id.
func (table *Table) ID() int {
	return int(table.id)
}

// Name returns the table name.
func (table *Table) Name() string {
	return C.GoString(C.bpf_table_name(table.module.p, table.id))
}

// Config returns the table properties (name, fd, ...).
func (table *Table) Config() *Config {
	mod := table.module.p
	return &Config{
		Name:     C.GoString(C.bpf_table_name(mod, table.id)),
		Fd:       int(C.bpf_table_fd_id(mod, table.id)),
		KeySize:  uint64(C.bpf_table_key_size_id(mod, table.id)),
		LeafSize: uint64(C.bpf_table_leaf_size_id(mod, table.id)),
		KeyDesc:  C.GoString(C.bpf_table_key_desc_id(mod, table.id)),
		LeafDesc: C.GoString(C.bpf_table_leaf_desc_id(mod, table.id)),
		Capacity: int(C.bpf_table_max_entries_id(mod, table.id)),
	}
}

func (table *Table) LeafStrToBytes(leafStr string) ([]byte, error) {
	mod := table.module.p

	leafSize := C.bpf_table_leaf_size_id(mod, table.id)
	leaf := make([]byte, leafSize)
	leafP := unsafe.Pointer(&leaf[0])

	leafCS := C.CString(leafStr)
	defer C.free(unsafe.Pointer(leafCS))

	r := C.bpf_table_leaf_sscanf(mod, table.id, leafCS, leafP)
	if r != 0 {
		return nil, fmt.Errorf("error scanning leaf (%v) from string", leafStr)
	}
	return leaf, nil
}

func (table *Table) KeyStrToBytes(keyStr string) ([]byte, error) {
	mod := table.module.p

	keySize := C.bpf_table_key_size_id(mod, table.id)
	key := make([]byte, keySize)
	keyP := unsafe.Pointer(&key[0])

	keyCS := C.CString(keyStr)
	defer C.free(unsafe.Pointer(keyCS))

	r := C.bpf_table_key_sscanf(mod, table.id, keyCS, keyP)
	if r != 0 {
		return nil, fmt.Errorf("error scanning key (%v) from string", keyStr)
	}
	return key, nil
}

// KeyBytesToStr returns the given key value formatted using the bcc-table's key string printer.
func (table *Table) KeyBytesToStr(key []byte) (string, error) {
	keySize := len(key)
	keyP := unsafe.Pointer(&key[0])

	keyStr := make([]byte, keySize*8)
	keyStrP := (*C.char)(unsafe.Pointer(&keyStr[0]))

	if res := C.bpf_table_key_snprintf(table.module.p, table.id, keyStrP, C.size_t(len(keyStr)), keyP); res != 0 {
		return "", fmt.Errorf("formatting table-key: %d", res)
	}

	return string(keyStr[:bytes.IndexByte(keyStr, 0)]), nil
}

// LeafBytesToStr returns the given leaf value formatted using the bcc-table's leaf string printer.
func (table *Table) LeafBytesToStr(leaf []byte) (string, error) {
	leafSize := len(leaf)
	leafP := unsafe.Pointer(&leaf[0])

	leafStr := make([]byte, leafSize*8)
	leafStrP := (*C.char)(unsafe.Pointer(&leafStr[0]))

	if res := C.bpf_table_leaf_snprintf(table.module.p, table.id, leafStrP, C.size_t(len(leafStr)), leafP); res != 0 {
		return "", fmt.Errorf("formatting table-leaf: %d", res)
	}

	return string(leafStr[:bytes.IndexByte(leafStr, 0)]), nil
}

// Get takes a key and returns the value or nil, and an 'ok' style indicator.
func (table *Table) Get(key []byte) ([]byte, error) {
	mod := table.module.p
	fd := C.bpf_table_fd_id(mod, table.id)

	keyP := unsafe.Pointer(&key[0])

	leafSize := C.bpf_table_leaf_size_id(mod, table.id)
	mapType := C.bpf_table_type_id(mod, table.id)
	switch mapType {
	case C.BPF_MAP_TYPE_PERCPU_HASH, C.BPF_MAP_TYPE_PERCPU_ARRAY:
		cpus, err := cpupossible.Get()
		if err != nil {
			return nil, fmt.Errorf("get possible cpus: %w", err)
		}
		leafSize *= C.size_t(len(cpus))
	}
	leaf := make([]byte, leafSize)
	leafP := unsafe.Pointer(&leaf[0])

	r, err := C.bpf_lookup_elem(fd, keyP, leafP)
	if r != 0 {
		keyStr, errK := table.KeyBytesToStr(key)
		if errK != nil {
			keyStr = fmt.Sprintf("%v", key)
		}
		return nil, fmt.Errorf("Table.Get: key %v: %v", keyStr, err)
	}

	return leaf, nil
}

// GetP takes a key and returns the value or nil.
func (table *Table) GetP(key unsafe.Pointer) (unsafe.Pointer, error) {
	fd := C.bpf_table_fd_id(table.module.p, table.id)

	leafSize := C.bpf_table_leaf_size_id(table.module.p, table.id)
	mapType := C.bpf_table_type_id(table.module.p, table.id)
	switch mapType {
	case C.BPF_MAP_TYPE_PERCPU_HASH, C.BPF_MAP_TYPE_PERCPU_ARRAY:
		cpus, err := cpupossible.Get()
		if err != nil {
			return nil, fmt.Errorf("get possible cpus: %w", err)
		}
		leafSize *= C.size_t(len(cpus))
	}
	leaf := make([]byte, leafSize)
	leafP := unsafe.Pointer(&leaf[0])

	_, err := C.bpf_lookup_elem(fd, key, leafP)
	if err != nil {
		return nil, err
	}
	return leafP, nil
}

// Set a key to a value.
func (table *Table) Set(key, leaf []byte) error {
	fd := C.bpf_table_fd_id(table.module.p, table.id)

	keyP := unsafe.Pointer(&key[0])
	leafP := unsafe.Pointer(&leaf[0])

	r, err := C.bpf_update_elem(fd, keyP, leafP, 0)
	if r != 0 {
		keyStr, errK := table.KeyBytesToStr(key)
		if errK != nil {
			keyStr = fmt.Sprintf("%v", key)
		}
		leafStr, errL := table.LeafBytesToStr(leaf)
		if errL != nil {
			leafStr = fmt.Sprintf("%v", leaf)
		}

		return fmt.Errorf("Table.Set: update %v to %v: %v", keyStr, leafStr, err)
	}

	return nil
}

// SetP a key to a value as unsafe.Pointer.
func (table *Table) SetP(key, leaf unsafe.Pointer) error {
	fd := C.bpf_table_fd_id(table.module.p, table.id)

	_, err := C.bpf_update_elem(fd, key, leaf, 0)
	if err != nil {
		return err
	}

	return nil
}

// Delete a key.
func (table *Table) Delete(key []byte) error {
	fd := C.bpf_table_fd_id(table.module.p, table.id)
	keyP := unsafe.Pointer(&key[0])
	r, err := C.bpf_delete_elem(fd, keyP)
	if r != 0 {
		keyStr, errK := table.KeyBytesToStr(key)
		if errK != nil {
			keyStr = fmt.Sprintf("%v", key)
		}
		return fmt.Errorf("Table.Delete: key %v: %v", keyStr, err)
	}
	return nil
}

// DeleteP a key.
func (table *Table) DeleteP(key unsafe.Pointer) error {
	fd := C.bpf_table_fd_id(table.module.p, table.id)
	_, err := C.bpf_delete_elem(fd, key)
	if err != nil {
		return err
	}
	return nil
}

// DeleteAll deletes all entries from the table
func (table *Table) DeleteAll() error {
	mod := table.module.p
	fd := C.bpf_table_fd_id(mod, table.id)

	keySize := C.bpf_table_key_size_id(mod, table.id)
	key := make([]byte, keySize)
	keyP := unsafe.Pointer(&key[0])
	for res := C.bpf_get_first_key(fd, keyP, keySize); res == 0; res = C.bpf_get_next_key(fd, keyP, keyP) {
		r, err := C.bpf_delete_elem(fd, keyP)
		if r != 0 {
			return fmt.Errorf("Table.DeleteAll: unable to delete element: %v", err)
		}
	}
	return nil
}

// From src/cc/export/helpers.h
// This must be always sync with BPF.h
const BPF_MAX_STACK_DEPTH = 127

func (table *Table) ClearStackId(stackId int) {
	if stackId > 0 {
		table.Remove(unsafe.Pointer(&stackId))
	}
}

func (table *Table) GetStackAddr(stackId int, clear bool) []uintptr {
	if stackId < 0 {
		return nil
	}

	var res []uintptr

	ptr, err := table.GetP(unsafe.Pointer(&stackId))
	if err != nil {
		return res
	}

	stack := (*C.struct_stacktrace_t)(ptr)

	for i := 0; (i < BPF_MAX_STACK_DEPTH) && (stack.ip[i] != 0); i++ {
		res = append(res, uintptr(stack.ip[i]))
	}
	if clear {
		table.Remove(unsafe.Pointer(&stackId))
	}
	return res
}

func (table *Table) GetAddrSymbol(addr uintptr, pid int) string {
	if pid < 0 {
		pid = -1
	}
	cache, ok := table.pidSym[pid]
	if !ok {
		cache = C.bcc_symcache_new(C.int(pid), &table.symbolOpt)
		table.pidSym[pid] = cache
	}

	var sym C.struct_bcc_symbol
	var s string
	ret := C.bcc_symcache_resolve(cache, C.uint64_t(addr), &sym)
	if ret < 0 {
		s = "[UNKNOWN]"
	} else {
		s = C.GoString(sym.demangle_name)
		C.bcc_symbol_free_demangle_name(&sym)
	}
	return s
}

func (table *Table) Lookup(key, value unsafe.Pointer) error {
	_, err := C.bpf_lookup_elem(table.fd, key, value)
	return err
}

func (table *Table) Remove(key unsafe.Pointer) error {
	_, err := C.bpf_delete_elem(table.fd, key)
	return err
}

func (table *Table) Update(key, value unsafe.Pointer) error {
	_, err := C.bpf_update_elem(table.fd, key, value, 0)
	return err
}

func (table *Table) Next(key, nextKey unsafe.Pointer) error {
	_, err := C.bpf_get_next_key(table.fd, key, nextKey)
	return err
}

func (table *Table) First(key unsafe.Pointer) bool {
	return C.bpf_get_first_key(table.fd, key, C.bpf_table_key_size_id(table.module.p, table.id)) >= 0
}

// TableIterator contains the current position for iteration over a *bcc.Table and provides methods for iteration.
type TableIterator struct {
	table *Table
	fd    C.int

	err error

	key  []byte
	leaf []byte
}

// Iter returns an iterator to list all table entries available as raw bytes.
func (table *Table) Iter() *TableIterator {
	fd := C.bpf_table_fd_id(table.module.p, table.id)

	return &TableIterator{
		table: table,
		fd:    fd,
	}
}

// Next looks up the next element and return true if one is available.
func (it *TableIterator) Next() bool {
	if it.err != nil {
		return false
	}

	if it.key == nil {
		keySize := C.bpf_table_key_size_id(it.table.module.p, it.table.id)

		key := make([]byte, keySize)
		keyP := unsafe.Pointer(&key[0])
		if res, err := C.bpf_get_first_key(it.fd, keyP, keySize); res != 0 {
			if !os.IsNotExist(err) {
				it.err = err
			}
			return false
		}

		leafSize := C.bpf_table_leaf_size_id(it.table.module.p, it.table.id)
		mapType := C.bpf_table_type_id(it.table.module.p, it.table.id)
		switch mapType {
		case C.BPF_MAP_TYPE_PERCPU_HASH, C.BPF_MAP_TYPE_PERCPU_ARRAY:
			cpus, err := cpupossible.Get()
			if err != nil {
				it.err = fmt.Errorf("get possible cpus: %w", err)
				return false
			}
			leafSize *= C.size_t(len(cpus))
		}
		leaf := make([]byte, leafSize)

		it.key = key
		it.leaf = leaf
	} else {
		keyP := unsafe.Pointer(&it.key[0])
		if res, err := C.bpf_get_next_key(it.fd, keyP, keyP); res != 0 {
			if !os.IsNotExist(err) {
				it.err = err
			}
			return false
		}
	}

	keyP := unsafe.Pointer(&it.key[0])
	leafP := unsafe.Pointer(&it.leaf[0])
	if res, err := C.bpf_lookup_elem(it.fd, keyP, leafP); res != 0 {
		it.err = errIterationFailed
		if !os.IsNotExist(err) {
			it.err = err
		}
		return false
	}

	return true
}

// Key returns the current key value of the iterator, if the most recent call to Next returned true.
// The slice is valid only until the next call to Next.
func (it *TableIterator) Key() []byte {
	return it.key
}

// Leaf returns the current leaf value of the iterator, if the most recent call to Next returned true.
// The slice is valid only until the next call to Next.
func (it *TableIterator) Leaf() []byte {
	return it.leaf
}

// Err returns the last error that ocurred while table.Iter oder iter.Next
func (it *TableIterator) Err() error {
	return it.err
}

func boolToInt(val bool) int {
	if val {
		return 1
	}
	return 0
}
