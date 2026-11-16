package manifest

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// isLibrarySuffix is intentionally independent of target. It is used to
// reject stray files in the planner's private artifact root.
func isLibrarySuffix(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".dll", ".so", ".dylib":
		return true
	default:
		return false
	}
}

func validateLibrarySuffix(path string, target Target) error {
	want := map[string]string{"windows": ".dll", "linux": ".so", "darwin": ".dylib"}[target.OS]
	if want == "" {
		return fmt.Errorf("unsupported target OS %q", target.OS)
	}
	if strings.ToLower(filepath.Ext(path)) != want {
		return fmt.Errorf("library %q must have a %s extension for %s", path, want, target.OS)
	}
	return nil
}

func validateLibrary(path string, target Target) error {
	if err := validateLibrarySuffix(path, target); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open library: %w", err)
	}
	defer f.Close()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("library header is truncated: %w", err)
	}
	switch target.OS {
	case "windows":
		if magic[0] != 'M' || magic[1] != 'Z' {
			return fmt.Errorf("expected a PE DLL for %s", target.Key())
		}
		return validatePEDLL(f, target.Arch)
	case "linux":
		if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
			return fmt.Errorf("expected an ELF shared object for %s", target.Key())
		}
		return validateELFSharedObject(f, target.Arch)
	case "darwin":
		return validateMachODylib(f, target.Arch)
	default:
		return fmt.Errorf("unsupported target OS %q", target.OS)
	}
}

func validatePEDLL(f *os.File, arch string) error {
	library, err := pe.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse PE DLL: %w", err)
	}
	defer library.Close()
	if library.OptionalHeader == nil || library.Characteristics&pe.IMAGE_FILE_DLL == 0 {
		return fmt.Errorf("PE image is not a DLL")
	}
	wantMachine, ok := map[string]uint16{
		"386": pe.IMAGE_FILE_MACHINE_I386, "amd64": pe.IMAGE_FILE_MACHINE_AMD64,
		"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
	}[arch]
	if !ok {
		return fmt.Errorf("unsupported PE architecture %q", arch)
	}
	if library.Machine != wantMachine {
		return fmt.Errorf("PE machine is 0x%04x, expected 0x%04x", library.Machine, wantMachine)
	}
	is64 := arch != "386"
	if _, ok := library.OptionalHeader.(*pe.OptionalHeader64); ok != is64 {
		return fmt.Errorf("PE optional header width does not match %s", arch)
	}
	return nil
}

func validateELFSharedObject(f *os.File, arch string) error {
	library, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse ELF shared object: %w", err)
	}
	defer library.Close()
	if library.Type != elf.ET_DYN {
		return fmt.Errorf("ELF file type is %s, expected ET_DYN", library.Type)
	}
	wantMachine, ok := map[string]elf.Machine{
		"386": elf.EM_386, "amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64,
	}[arch]
	if !ok {
		return fmt.Errorf("unsupported ELF architecture %q", arch)
	}
	if library.Machine != wantMachine {
		return fmt.Errorf("ELF architecture is %s, expected %s", library.Machine, wantMachine)
	}
	wantClass := elf.ELFCLASS64
	if arch == "386" {
		wantClass = elf.ELFCLASS32
	}
	if library.Class != wantClass {
		return fmt.Errorf("ELF class is %s, expected %s for %s", library.Class, wantClass, arch)
	}
	return nil
}

func validateMachODylib(f *os.File, arch string) error {
	wantCPU, ok := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[arch]
	if !ok {
		return fmt.Errorf("unsupported Mach-O architecture %q", arch)
	}
	fat, err := macho.NewFatFile(f)
	if err == nil {
		defer fat.Close()
		for _, image := range fat.Arches {
			if image.Cpu == wantCPU {
				if image.Type != macho.TypeDylib {
					return fmt.Errorf("Mach-O file type is %s, expected dylib", image.Type)
				}
				return nil
			}
		}
		return fmt.Errorf("fat Mach-O library does not contain %s architecture", arch)
	}
	if !errors.Is(err, macho.ErrNotFat) {
		return fmt.Errorf("parse Mach-O dylib: %w", err)
	}
	library, err := macho.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse Mach-O dylib: %w", err)
	}
	defer library.Close()
	if library.Type != macho.TypeDylib {
		return fmt.Errorf("Mach-O file type is %s, expected dylib", library.Type)
	}
	if library.Cpu != wantCPU {
		return fmt.Errorf("Mach-O architecture is %s, expected %s", library.Cpu, wantCPU)
	}
	return nil
}
