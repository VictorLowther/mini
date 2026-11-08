package model

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// Stats accumulates cost and call counts across every model instance, like
// GlobalModelStats, and enforces the optional process-wide limits set through
// the environment.
type Stats struct {
	mu         sync.Mutex
	cost       float64
	nCalls     int
	tokens     int
	costLimit  float64
	callLimit  int
	limitsRead bool
}

// GLOBAL_STATS is the process-wide tracker.
var GLOBAL_STATS = &Stats{}

// Add records one completed call and reports a limit breach. The comparison is
// strict (`<` after accumulating), i.e. the limit fires once the total exceeds
// it, matching Python's `0 < limit < total`.
func (s *Stats) Add(cost float64, tokens int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.limitsRead {
		s.costLimit = envFloat("MINI_GLOBAL_COST_LIMIT")
		s.callLimit = envInt("MINI_GLOBAL_CALL_LIMIT")
		s.limitsRead = true
	}
	s.cost += cost
	s.tokens += tokens
	s.nCalls++
	if 0 < s.costLimit && s.costLimit < s.cost {
		return fmt.Errorf("global cost/call limit exceeded: $%.4f / %d", s.cost, s.nCalls)
	}
	if 0 < s.callLimit && s.callLimit < s.nCalls+1 {
		return fmt.Errorf("global cost/call limit exceeded: $%.4f / %d", s.cost, s.nCalls)
	}
	return nil
}

// Reset clears the totals; used by tests, which share the process.
func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cost, s.nCalls, s.tokens, s.limitsRead = 0, 0, 0, false
}

func envFloat(key string) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return 0
}

func envInt(key string) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return 0
}
