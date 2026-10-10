// Package transport schedules WebSocket messages without changing stream order.
package transport

import "time"

type Event int

const (
	Message Event = iota
	Tick
	Closed
)
const InteractiveBurst = 8

// Next prefers interactive traffic but reserves one ready bulk message after
// eight interactive messages. Each lane is FIFO; session states and terminal
// bytes must share a lane, as must a bulk stream's data and lifecycle messages.
func Next[T any](interactive, bulk <-chan T, done <-chan struct{}, ticks <-chan time.Time, burst *int) (T, Event) {
	var zero T
	select {
	case <-done:
		return zero, Closed
	default:
	}
	select {
	case <-ticks:
		return zero, Tick
	default:
	}
	if *burst >= InteractiveBurst {
		select {
		case out, ok := <-bulk:
			if !ok {
				return zero, Closed
			}
			*burst = 0
			return out, Message
		default:
		}
	}
	select {
	case out, ok := <-interactive:
		if !ok {
			return zero, Closed
		}
		*burst++
		return out, Message
	default:
	}
	select {
	case <-done:
		return zero, Closed
	case <-ticks:
		return zero, Tick
	case out, ok := <-interactive:
		if !ok {
			return zero, Closed
		}
		*burst++
		return out, Message
	case out, ok := <-bulk:
		if !ok {
			return zero, Closed
		}
		*burst = 0
		return out, Message
	}
}
