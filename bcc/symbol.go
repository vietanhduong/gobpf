// Copyright 2017 Louis McCormack
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
	"fmt"
	"regexp"
	"sync"
	"unsafe"
)

/*
#cgo CFLAGS: -I/usr/include/bcc/compat
#cgo LDFLAGS: -lbcc
#include <bcc/bcc_common.h>
#include <bcc/libbpf.h>
#include <bcc/bcc_syms.h>
#include <linux/bpf.h>
#include <linux/elf.h>

extern void foreach_symbol_callback(char*, uint64_t);
*/
import "C"

type Symbolizer struct {
	bccSymbolCache map[int]unsafe.Pointer
	bccSymbolOpt   C.struct_bcc_symbol_option
}

func NewSymbolizer() *Symbolizer {
	return &Symbolizer{
		bccSymbolCache: make(map[int]unsafe.Pointer),
		bccSymbolOpt: C.struct_bcc_symbol_option{
			use_debug_file:       C.int(boolToInt(false)),
			check_debug_file_crc: C.int(boolToInt(false)),
			lazy_symbolize:       C.int(boolToInt(true)),
			use_symbol_type:      (1 << C.STT_FUNC) | (1 << C.STT_GNU_IFUNC),
		},
	}
}

func (s *Symbolizer) SymbolOrAddrIfUnknown(pid int, addr uintptr) string {
	symbol := &C.struct_bcc_symbol{}
	cache := s.getBCCSymbolCache(pid)
	resolved := C.bcc_symcache_resolve(cache, C.uint64_t(addr), symbol)
	if resolved == 0 {
		name := symbol.demangle_name
		C.bcc_symbol_free_demangle_name(symbol)
		return C.GoString(name)
	}

	if module := C.GoString(symbol.module); module != "" {
		return s.formatModuleName(C.GoString(symbol.module), uintptr(symbol.offset))
	}
	return s.formatAddress(addr)
}

func (s *Symbolizer) ReleasePidSymCache(pid int) {
	if cache, ok := s.bccSymbolCache[pid]; ok {
		C.bcc_free_symcache(cache, C.int(pid))
		delete(s.bccSymbolCache, pid)
	}
}

func (s *Symbolizer) getBCCSymbolCache(pid int) unsafe.Pointer {
	cache, ok := s.bccSymbolCache[pid]
	if ok {
		return cache
	}

	symbol := (*C.struct_bcc_symbol_option)(unsafe.Pointer(&s.bccSymbolOpt))
	cache = C.bcc_symcache_new(C.int(pid), symbol)
	s.bccSymbolCache[pid] = cache
	return cache
}

func (s *Symbolizer) formatAddress(addr uintptr) string {
	return fmt.Sprintf("0x%016x", addr)
}

func (s *Symbolizer) formatModuleName(module string, offset uintptr) string {
	return fmt.Sprintf("[m] %s + 0x%08x", module, offset)
}

type symbolAddress struct {
	name string
	addr uint64
}

// symbolCache will cache module lookups
var symbolCache = struct {
	cache         map[string][]*symbolAddress
	currentModule string
	lock          *sync.Mutex
}{
	cache:         map[string][]*symbolAddress{},
	currentModule: "",
	lock:          &sync.Mutex{},
}

type bccSymbol struct {
	name         *C.char
	demangleName *C.char
	module       *C.char
	offset       C.ulonglong
}

type bccSymbolOption struct {
	useDebugFile      int
	checkDebugFileCrc int
	lazySymbolize     int
	useSymbolType     uint32
}

// resolveSymbolPath returns the file and offset to locate symname in module
func resolveSymbolPath(module string, symname string, addr uint64, pid int) (string, uint64, error) {
	if pid == -1 {
		pid = 0
	}

	modname, offset, err := bccResolveSymname(module, symname, addr, pid)
	if err != nil {
		return "", 0, fmt.Errorf("unable to locate symbol %s in module %s: %v", symname, module, err)
	}

	return modname, offset, nil
}

func bccResolveSymname(module string, symname string, addr uint64, pid int) (string, uint64, error) {
	symbol := &bccSymbol{}
	symbolC := (*C.struct_bcc_symbol)(unsafe.Pointer(symbol))
	moduleCS := C.CString(module)
	defer C.free(unsafe.Pointer(moduleCS))
	symnameCS := C.CString(symname)
	defer C.free(unsafe.Pointer(symnameCS))

	res, err := C.bcc_resolve_symname(moduleCS, symnameCS, (C.uint64_t)(addr), C.int(pid), nil, symbolC)
	if res < 0 {
		return "", 0, fmt.Errorf("unable to locate symbol %s in module %s: %v", symname, module, err)
	}

	return C.GoString(symbolC.module), (uint64)(symbolC.offset), nil
}

func bccResolveName(module, symname string, pid int) (uint64, error) {
	symbol := &bccSymbolOption{}
	symbolC := (*C.struct_bcc_symbol_option)(unsafe.Pointer(symbol))

	pidC := C.int(pid)
	cache := C.bcc_symcache_new(pidC, symbolC)
	defer C.bcc_free_symcache(cache, pidC)

	moduleCS := C.CString(module)
	defer C.free(unsafe.Pointer(moduleCS))

	nameCS := C.CString(symname)
	defer C.free(unsafe.Pointer(nameCS))

	var addr uint64
	addrC := C.uint64_t(addr)
	res := C.bcc_symcache_resolve_name(cache, moduleCS, nameCS, &addrC)
	if res < 0 {
		return 0, fmt.Errorf("unable to locate symbol %s in module %s", symname, module)
	}

	return addr, nil
}

// getUserSymbolsAndAddresses finds a list of symbols associated with a module,
// along with their addresses. The results are cached in the symbolCache and
// returned
func getUserSymbolsAndAddresses(module string) ([]*symbolAddress, error) {
	symbolCache.lock.Lock()
	defer symbolCache.lock.Unlock()
	// return previously cached list if it exists
	if _, ok := symbolCache.cache[module]; ok {
		return symbolCache.cache[module], nil
	}

	symbolCache.cache[module] = []*symbolAddress{}
	symbolCache.currentModule = module

	if err := bccForeachSymbol(module); err != nil {
		return nil, err
	}

	return symbolCache.cache[module], nil
}

func matchUserSymbols(module, match string) ([]*symbolAddress, error) {
	r, err := regexp.Compile(match)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %s : %s", match, err)
	}
	matchedSymbols := []*symbolAddress{}
	symbols, err := getUserSymbolsAndAddresses(module)
	if err != nil {
		return nil, err
	}
	for _, sym := range symbols {
		if r.MatchString(sym.name) {
			matchedSymbols = append(matchedSymbols, sym)
		}
	}
	return matchedSymbols, nil
}

// foreach_symbol_callback is a gateway function that will be exported to C
// so that it can be referenced as a function pointer
//
//export foreach_symbol_callback
func foreach_symbol_callback(symname *C.char, addr C.uint64_t) {
	symbolCache.cache[symbolCache.currentModule] = append(symbolCache.cache[symbolCache.currentModule], &symbolAddress{C.GoString(symname), (uint64)(addr)})
}

func bccForeachSymbol(module string) error {
	moduleCS := C.CString(module)
	defer C.free(unsafe.Pointer(moduleCS))
	res := C.bcc_foreach_function_symbol(moduleCS, (C.SYM_CB)(unsafe.Pointer(C.foreach_symbol_callback)))
	if res < 0 {
		return fmt.Errorf("unable to list symbols for %s", module)
	}
	return nil
}
