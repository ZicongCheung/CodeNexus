package api

import (
	"strings"
	"testing"
)

func TestSyntheticSignatureIsStableAndNamespaced(t *testing.T) {
	a := syntheticSignature("let me think")
	if a != syntheticSignature("let me think") {
		t.Fatal("the same thinking must yield the same signature")
	}
	if a == syntheticSignature("a different thought") {
		t.Fatal("different thinking must yield different signatures")
	}
	if !strings.HasPrefix(a, "cnx-") {
		t.Fatalf("signature %q must be namespaced so it cannot pass for a real one", a)
	}
}
