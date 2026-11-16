package manifest

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestValidateLibraryFormats(t *testing.T) {
	workspace := t.TempDir()
	pe386 := writePEDLL(t, workspace, "windows-386.dll", 0x014c)
	elfAMD64 := writeELFSharedObject(t, workspace, "linux-amd64.so", 2, 62)
	machoARM64 := writeMachODylib(t, workspace, "darwin-arm64.dylib", 0x0100000c)

	for _, test := range []struct {
		name   string
		path   string
		target Target
	}{
		{"PE 386 DLL", pe386, Target{OS: "windows", Arch: "386"}},
		{"ELF amd64 shared object", elfAMD64, Target{OS: "linux", Arch: "amd64"}},
		{"Mach-O arm64 dylib", machoARM64, Target{OS: "darwin", Arch: "arm64"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLibrary(test.path, test.target); err != nil {
				t.Fatalf("validateLibrary() error = %v", err)
			}
		})
	}
}

func TestValidateLibraryRejectsWrongFormatAndArchitecture(t *testing.T) {
	workspace := t.TempDir()
	pe386 := writePEDLL(t, workspace, "windows-386.dll", 0x014c)
	elfAMD64 := writeELFSharedObject(t, workspace, "linux-amd64.so", 2, 62)
	machoARM64 := writeMachODylib(t, workspace, "darwin-arm64.dylib", 0x0100000c)
	elfObject := writeELFSharedObject(t, workspace, "relocatable.so", 2, 62)
	data := readFile(t, elfObject)
	binary.LittleEndian.PutUint16(data[16:18], 1) // ET_REL
	writeFile(t, workspace, "relocatable.so", data)

	for _, test := range []struct {
		name, path, want string
		target           Target
	}{
		{"PE wrong machine", pe386, "PE machine", Target{OS: "windows", Arch: "amd64"}},
		{"ELF wrong machine", elfAMD64, "ELF architecture", Target{OS: "linux", Arch: "arm64"}},
		{"ELF relocatable", elfObject, "expected ET_DYN", Target{OS: "linux", Arch: "amd64"}},
		{"Mach-O wrong CPU", machoARM64, "Mach-O architecture", Target{OS: "darwin", Arch: "amd64"}},
		{"ELF as Windows", elfAMD64, "must have a .dll", Target{OS: "windows", Arch: "amd64"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateLibrary(test.path, test.target)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateLibrary() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func writePEDLL(t *testing.T, workspace, relative string, machine uint16) string {
	t.Helper()
	is64 := machine != 0x014c
	optionalSize := 96
	magic := uint16(0x10b)
	if is64 {
		optionalSize = 112
		magic = 0x20b
	}
	data := make([]byte, 0x80+4+20+optionalSize)
	copy(data[:2], "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:0x40], 0x80)
	copy(data[0x80:0x84], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x84:0x86], machine)
	binary.LittleEndian.PutUint16(data[0x94:0x96], uint16(optionalSize))
	binary.LittleEndian.PutUint16(data[0x96:0x98], 0x2000) // IMAGE_FILE_DLL
	binary.LittleEndian.PutUint16(data[0x98:0x9a], magic)
	return writeFile(t, workspace, relative, data)
}

func writeELFSharedObject(t *testing.T, workspace, relative string, class byte, machine uint16) string {
	t.Helper()
	size := 64
	if class == 1 {
		size = 52
	}
	data := make([]byte, size)
	copy(data[0:4], []byte{0x7f, 'E', 'L', 'F'})
	data[4] = class
	data[5] = 1                                   // little endian
	data[6] = 1                                   // ELF version
	binary.LittleEndian.PutUint16(data[16:18], 3) // ET_DYN
	binary.LittleEndian.PutUint16(data[18:20], machine)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	if class == 1 {
		binary.LittleEndian.PutUint16(data[40:42], uint16(size))
	} else {
		binary.LittleEndian.PutUint16(data[52:54], uint16(size))
	}
	return writeFile(t, workspace, relative, data)
}

func writeMachODylib(t *testing.T, workspace, relative string, cpu uint32) string {
	t.Helper()
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(data[4:8], cpu)
	binary.LittleEndian.PutUint32(data[8:12], 0)
	binary.LittleEndian.PutUint32(data[12:16], 6) // MH_DYLIB
	return writeFile(t, workspace, relative, data)
}
