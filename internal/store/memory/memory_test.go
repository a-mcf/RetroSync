package memory_test

import (
	"testing"

	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
	"github.com/a-mcf/retrosync/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return memory.New()
	})
}
