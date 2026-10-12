package proxy

import "testing"

// seeProcess tells OnProcess about each real process, and nothing about pid 0
// (a connection the kernel could not attribute); with no OnProcess set it does
// nothing.
func TestSeeProcessTellsTheBuildEachProcess(t *testing.T) {
	prev := OnProcess
	t.Cleanup(func() { OnProcess = prev })

	OnProcess = nil
	seeProcess(7)

	var seen []uint32
	OnProcess = func(pid uint32) { seen = append(seen, pid) }
	seeProcess(0)
	seeProcess(42)
	seeProcess(42)
	if len(seen) != 2 || seen[0] != 42 || seen[1] != 42 {
		t.Fatalf("OnProcess saw %v", seen)
	}
}
