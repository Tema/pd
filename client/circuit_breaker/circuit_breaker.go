// Copyright 2024 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package circuit_breaker

import (
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	m "github.com/tikv/pd/client/metrics"
	"go.uber.org/zap"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/log"
)

// ErrOpenState is returned when the CircuitBreaker is open or half-open with pending requests.
var ErrOpenState = errors.New("circuit breaker is open")

// Overloading is a type describing service return value
type Overloading int

const (
	// No means the service is not overloaded
	No Overloading = iota
	// Yes means the service is overloaded
	Yes
)

// Settings describes configuration for Circuit Breaker
type Settings struct {
	// Defines the error rate threshold to trip the circuit breaker.
	ErrorRateThresholdPct uint32
	// Defines the average qps over the `error_rate_window` that must be met before evaluating the error rate threshold.
	MinQPSForOpen uint32
	// Defines how long to track errors before evaluating error_rate_threshold.
	ErrorRateWindow time.Duration
	// Defines how long to wait after circuit breaker is open before go to half-open state to send a probe request.
	CoolDownInterval time.Duration
	// Defines how many subsequent requests to test after cooldown period before fully close the circuit.
	HalfOpenSuccessCount uint32
}

// CircuitBreaker is a state machine to prevent sending requests that are likely to fail.
type CircuitBreaker[T any] struct {
	config *Settings
	name   string

	mutex sync.Mutex
	state *State[T]

	successCounter  prometheus.Counter
	failureCounter  prometheus.Counter
	fastFailCounter prometheus.Counter
}

// StateType is a type that represents a state of CircuitBreaker.
type StateType int

// States of CircuitBreaker.
const (
	StateClosed StateType = iota
	StateOpen
	StateHalfOpen
)

// String implements stringer interface.
func (s StateType) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return fmt.Sprintf("unknown state: %d", s)
	}
}

var replacer = strings.NewReplacer(" ", "_", "-", "_")

// NewCircuitBreaker returns a new CircuitBreaker configured with the given Settings.
func NewCircuitBreaker[T any](name string, st Settings) *CircuitBreaker[T] {
	cb := new(CircuitBreaker[T])
	cb.name = name
	cb.config = &st
	cb.state = cb.newState(time.Now(), StateClosed)

	metricName := replacer.Replace(name)
	cb.successCounter = m.CircuitBreakerCounters.WithLabelValues(metricName, "success")
	cb.failureCounter = m.CircuitBreakerCounters.WithLabelValues(metricName, "failure")
	cb.fastFailCounter = m.CircuitBreakerCounters.WithLabelValues(metricName, "fast_fail")
	return cb
}

// ChangeSettings changes the CircuitBreaker settings.
// The changes will be reflected only in the next evaluation window.
func (cb *CircuitBreaker[T]) ChangeSettings(apply func(config *Settings)) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	apply(cb.config)
}

// Execute calls the given function if the CircuitBreaker is closed and returns the result of execution.
// Execute returns an error instantly if the CircuitBreaker is open.
// https://github.com/tikv/rfcs/blob/master/text/0115-circuit-breaker.md
func (cb *CircuitBreaker[T]) Execute(call func() (T, error, Overloading)) (T, error) {
	result, err := cb.ExecuteAny(func() (interface{}, error, Overloading) {
		res, err, open := call()
		return res, err, open
	})
	if result == nil {
		// this branch is required to support primitive types like int, which can't be nil
		var defaultValue T
		return defaultValue, err
	} else {
		return result.(T), err
	}
}

// ExecuteAny is similar to Execute, but allows the caller to return any type of result.
func (cb *CircuitBreaker[T]) ExecuteAny(call func() (interface{}, error, Overloading)) (interface{}, error) {
	state, err := cb.onRequest()
	if err != nil {
		var defaultValue interface{}
		return defaultValue, err
	}

	defer func() {
		e := recover()
		if e != nil {
			cb.onResult(state, Yes)
			panic(e)
		}
	}()

	result, err, open := call()
	cb.onResult(state, open)
	return result, err
}

func (cb *CircuitBreaker[T]) onRequest() (*State[T], error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	state, err := cb.state.onRequest(cb)
	cb.state = state
	return state, err
}

func (cb *CircuitBreaker[T]) onResult(state *State[T], open Overloading) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	if cb.state == state {
		state.onResult(open)
	} // else the state moved forward so we don't need to update the counts
}

type State[T any] struct {
	stateType StateType
	cb        *CircuitBreaker[T]
	end       time.Time

	pendingCount uint32
	successCount uint32
	failureCount uint32
}

// newState creates a new State with the given configuration and reset all success/failure counters.
func (cb *CircuitBreaker[T]) newState(now time.Time, stateType StateType) *State[T] {
	var end time.Time
	var pendingCount uint32
	switch stateType {
	case StateClosed:
		end = now.Add(cb.config.ErrorRateWindow)
	case StateOpen:
		end = now.Add(cb.config.CoolDownInterval)
	case StateHalfOpen:
		// we transition to HalfOpen state on the first request after the cooldown period,
		//so we start with 1 pending request
		pendingCount = 1
	default:
		panic("unknown state")
	}
	return &State[T]{
		cb:           cb,
		stateType:    stateType,
		pendingCount: pendingCount,
		end:          end,
	}
}

// onRequest transitions the state to the next state based on the current state and the previous requests results
// All state transitions happens at the request evaluation time only
// The implementation represents a state machine effectively
func (s *State[T]) onRequest(cb *CircuitBreaker[T]) (*State[T], error) {
	var now = time.Now()
	switch s.stateType {
	case StateClosed:
		if s.end.Before(now) {
			// ErrorRateWindow is over, let's evaluate the error rate
			total := s.failureCount + s.successCount
			observedErrorRatePct := s.failureCount * 100 / total
			if s.cb.config.ErrorRateThresholdPct > 0 && total >= uint32(s.cb.config.ErrorRateWindow.Seconds())*s.cb.config.MinQPSForOpen && observedErrorRatePct >= s.cb.config.ErrorRateThresholdPct {
				// the error threshold is breached, let's move to open state and start failing all requests
				log.Error("Circuit breaker tripped. Starting to fail all requests",
					zap.String("name", cb.name),
					zap.Uint32("observedErrorRatePct", observedErrorRatePct),
					zap.String("config", fmt.Sprintf("%+v", cb.config)))
				cb.fastFailCounter.Inc()
				return cb.newState(now, StateOpen), ErrOpenState
			} else {
				// the error threshold is not breached or there were not enough requests to evaluate it,
				// continue in the closed state and allow all requests
				return cb.newState(now, StateClosed), nil
			}
		} else {
			// continue in closed state till ErrorRateWindow is over
			return s, nil
		}
	case StateOpen:
		if s.end.Before(now) {
			// CoolDownInterval is over, it is time to transition to half-open state
			log.Info("Circuit breaker cooldown period is over. Transitioning to half-open state to test the service",
				zap.String("name", cb.name),
				zap.String("config", fmt.Sprintf("%+v", cb.config)))
			return cb.newState(now, StateHalfOpen), nil
		} else {
			// continue in the open state till CoolDownInterval is over
			cb.fastFailCounter.Inc()
			return s, ErrOpenState
		}
	case StateHalfOpen:
		// do we need some expire time here in case of one of pending requests is stuck forever?
		if s.failureCount > 0 {
			// there were some failures during half-open state, let's go back to open state to wait a bit longer
			log.Error("Circuit breaker goes from half-open to open again as errors persist and continue to fail all requests",
				zap.String("name", cb.name),
				zap.String("config", fmt.Sprintf("%+v", cb.config)))
			cb.fastFailCounter.Inc()
			return cb.newState(now, StateOpen), ErrOpenState
		} else if s.successCount == s.cb.config.HalfOpenSuccessCount {
			// all probe requests are succeeded, we can move to closed state and allow all requests
			log.Info("Circuit breaker is closed. Start allowing all requests",
				zap.String("name", cb.name),
				zap.String("config", fmt.Sprintf("%+v", cb.config)))
			return cb.newState(now, StateClosed), nil
		} else if s.pendingCount < s.cb.config.HalfOpenSuccessCount {
			// allow more probe requests and continue in half-open state
			s.pendingCount++
			return s, nil
		} else {
			// continue in half-open state till all probe requests are done and fail all other requests for now
			cb.fastFailCounter.Inc()
			return s, ErrOpenState
		}
	default:
		panic("unknown state")
	}
}

func (s *State[T]) onResult(open Overloading) {
	switch open {
	case No:
		s.successCount++
		s.cb.successCounter.Inc()
	case Yes:
		s.failureCount++
		s.cb.fastFailCounter.Inc()
	default:
		panic("unknown state")
	}
}
