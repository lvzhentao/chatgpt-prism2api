package scheduler

import (
	"math"
	"sort"
	"sync"

	"prism-2api/internal/pool"
)

type roundRobinState struct {
	mu      sync.Mutex
	cursors map[string]int
}

func (s *roundRobinState) pick(key string, accounts []*pool.Account) *pool.Account {
	if len(accounts) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	index := s.cursors[key]
	if index >= 2_147_483_640 {
		index = 0
	}
	s.cursors[key] = index + 1
	return accounts[index%len(accounts)]
}

type smoothWeightedState struct {
	current map[string]int64
	weights map[string]int64
}

type weightedState struct {
	mu     sync.Mutex
	states map[string]*smoothWeightedState
}

func (s *weightedState) pick(key string, accounts []*pool.Account) *pool.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*smoothWeightedState)
	}
	st := s.states[key]
	if st == nil {
		st = &smoothWeightedState{}
		s.states[key] = st
	}
	weights := weightVector(accounts)
	if st.current == nil || !weightVectorsEqual(st.weights, weights) {
		st.current = make(map[string]int64)
	}
	st.weights = weights
	return pickSmoothWeighted(accounts, st.current)
}

func weightVector(accounts []*pool.Account) map[string]int64 {
	out := make(map[string]int64, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if w := a.Weight(); w > 0 {
			out[a.ID] = w
		}
	}
	return out
}

func weightVectorsEqual(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for id, w := range left {
		if right[id] != w {
			return false
		}
	}
	return true
}

// pickSmoothWeighted 移植自 CPA pickSmoothWeightedAuth：current += weight，选最大，再减 total。
func pickSmoothWeighted(accounts []*pool.Account, current map[string]int64) *pool.Account {
	active := make(map[string]struct{}, len(accounts))
	var picked *pool.Account
	var pickedCurrent int64
	var totalWeight int64
	for _, a := range accounts {
		if a == nil {
			continue
		}
		w := a.Weight()
		if w <= 0 {
			continue
		}
		active[a.ID] = struct{}{}
		current[a.ID] = saturatingAdd(current[a.ID], w)
		totalWeight = saturatingAdd(totalWeight, w)
		if picked == nil || current[a.ID] > pickedCurrent {
			picked = a
			pickedCurrent = current[a.ID]
		}
	}
	for id := range current {
		if _, ok := active[id]; !ok {
			delete(current, id)
		}
	}
	if picked == nil {
		return nil
	}
	current[picked.ID] = saturatingAdd(current[picked.ID], -totalWeight)
	return picked
}

func saturatingAdd(value, delta int64) int64 {
	if delta > 0 && value > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && value < math.MinInt64-delta {
		return math.MinInt64
	}
	return value + delta
}

func sortByID(accounts []*pool.Account) {
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
}

func highestPriority(accounts []*pool.Account) []*pool.Account {
	if len(accounts) <= 1 {
		return accounts
	}
	best := accounts[0].Priority()
	count := 1
	for _, a := range accounts[1:] {
		p := a.Priority()
		if p > best {
			best = p
			count = 1
		} else if p == best {
			count++
		}
	}
	if count == len(accounts) {
		return accounts
	}
	out := make([]*pool.Account, 0, count)
	for _, a := range accounts {
		if a.Priority() == best {
			out = append(out, a)
		}
	}
	return out
}

func positiveWeight(accounts []*pool.Account) []*pool.Account {
	out := make([]*pool.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Weight() > 0 {
			out = append(out, a)
		}
	}
	return out
}
