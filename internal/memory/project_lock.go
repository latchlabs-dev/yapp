package memory

import (
	"hash/fnv"
	"sync"
)

const projectLockStripes = 256

var projectLocks [projectLockStripes]sync.Mutex

// LockProject serializes destructive journal maintenance and appends for a
// project within this proxy process. It uses fixed stripes rather than a
// project-keyed map so attacker-controlled project names cannot grow memory.
func LockProject(project string) func() {
	idx := projectLockIndex(project)
	projectLocks[idx].Lock()
	return func() {
		projectLocks[idx].Unlock()
	}
}

func projectLockIndex(project string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(project))
	return h.Sum32() % projectLockStripes
}
