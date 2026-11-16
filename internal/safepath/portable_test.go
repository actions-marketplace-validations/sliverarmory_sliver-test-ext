package safepath

import "testing"

func TestValidatePortableRelative(t *testing.T) {
	for _, value := range []string{"library.so", ".libraries/linux/386/test.so", "nested/marker.txt", "unicode/café.txt"} {
		if err := ValidatePortableRelative(value); err != nil {
			t.Errorf("ValidatePortableRelative(%q) = %v", value, err)
		}
	}
	for _, value := range []string{
		"", "/absolute", "../escape", "nested/../escape", "nested\\escape", "C:/escape",
		"bad?.txt", "bad*.txt", "bad\".txt", "bad|.txt", "bad<.txt", "bad>.txt",
		"control\x01.txt", "CON.so", "nested/prn.txt", "dir./file.so", "trailing /file.so",
	} {
		if err := ValidatePortableRelative(value); err == nil {
			t.Errorf("ValidatePortableRelative(%q) unexpectedly succeeded", value)
		}
	}
}
