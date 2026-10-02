package updatefile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUpdateStateAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates", "state.json")
	if err := WriteJSON(path, map[string]int{"sequence": 0}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	finished := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-finished:
				return
			default:
				var result map[string]int
				if err := ReadJSON(path, &result); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}
	}()
	for i := 1; i <= 100; i++ {
		if err := WriteJSON(path, map[string]int{"sequence": i}); err != nil {
			close(finished)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(finished)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("reader observed partial or missing state: %v", err)
	default:
	}
	var result map[string]int
	if err := ReadJSON(path, &result); err != nil || result["sequence"] != 100 {
		t.Fatalf("final state: %v %v", result, err)
	}
}
func TestUpdateStateRejectsOversizedAndSymlinkFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	os.WriteFile(path, []byte(strings.Repeat("x", 65537)), 0600)
	var result any
	if err := ReadJSON(path, &result); err == nil {
		t.Fatal("oversized state accepted")
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(path, link); err == nil {
		if err := ReadJSON(link, &result); err == nil {
			t.Fatal("symlink state accepted")
		}
	}
}
