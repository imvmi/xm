package archive

import (
	"context"
	"testing"
)

func TestOrderedCapturesRestoresResponseOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan captured, 2)
	output := orderedCaptures(ctx, input)
	input <- captured{sequence: 1, data: []byte("second")}
	input <- captured{sequence: 0, data: []byte("first")}
	close(input)

	first, ok := <-output
	if !ok || string(first.data) != "first" {
		t.Fatalf("first response = %q, open=%t", first.data, ok)
	}
	second, ok := <-output
	if !ok || string(second.data) != "second" {
		t.Fatalf("second response = %q, open=%t", second.data, ok)
	}
	if _, ok = <-output; ok {
		t.Fatal("ordered stream did not close")
	}
}
