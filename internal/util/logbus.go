package util

import "sync"

type LogBus struct {
	mu       sync.Mutex
	buffer   []string
	maxLines int
	subs     map[chan string]struct{}
}

func NewLogBus(maxLines int) *LogBus {
	if maxLines <= 0 {
		maxLines = 100
	}
	return &LogBus{
		buffer:   make([]string, 0, maxLines),
		maxLines: maxLines,
		subs:     make(map[chan string]struct{}),
	}
}

func (b *LogBus) Publish(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffer = append(b.buffer, line)
	if len(b.buffer) > b.maxLines {
		b.buffer = b.buffer[len(b.buffer)-b.maxLines:]
	}

	for ch := range b.subs {
		select {
		case ch <- line:
		default:
		}
	}
}

func (b *LogBus) Snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.buffer))
	copy(out, b.buffer)
	return out
}

func (b *LogBus) Subscribe() chan string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan string, 256)
	b.subs[ch] = struct{}{}
	return ch
}

func (b *LogBus) Unsubscribe(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}
