package textutil

import "testing"

func TestCharacterOperations(t *testing.T) {
	if Substr("héllo", 1, 3) != "él" || Substr("abc", 5, 9) != "" || Substr("abc", 1, -1) != "bc" {
		t.Error("Substr works by character and clamps")
	}
	if Len("é😀") != 2 || Truncate("😀😀😀", 2) != "😀😀" {
		t.Error("Len and Truncate count characters")
	}
	if DecodeUTF8([]byte{'a', 0xff, 0xfe, 'b'}) != "a��b" {
		t.Error("one replacement character per invalid byte")
	}
}
