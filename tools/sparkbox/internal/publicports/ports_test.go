package publicports

import (
	"reflect"
	"testing"
)

func TestCommonHTTPS(t *testing.T) {
	want := []int{3000, 3001, 4000, 4200, 5000, 5173, 6006, 7860, 8000, 8080, 8443, 8501, 8888, 9000}
	if got := CommonHTTPS(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CommonHTTPS() = %v, want %v", got, want)
	}
	got := CommonHTTPS()
	got[0] = 1
	if CommonHTTPS()[0] != 3000 {
		t.Fatal("CommonHTTPS returned mutable package storage")
	}
	if got := HumanList(); got != "3000, 3001, 4000, 4200, 5000, 5173, 6006, 7860, 8000, 8080, 8443, 8501, 8888 and 9000" {
		t.Fatalf("HumanList() = %q", got)
	}
}
