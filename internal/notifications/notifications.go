// Package notifications owns daemon-level notifications shown to every
// connected Tandem UI. Unlike completed-turn notifications, these are not
// associated with a particular agent.
package notifications

import "sync"

type Action struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Primary bool   `json:"primary,omitempty"`
}

type Notification struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Message  string   `json:"message,omitempty"`
	Actions  []Action `json:"actions,omitempty"`
}

// Center is an in-memory, daemon-owned notification snapshot. Subscribers are
// called after mutations and receive a complete snapshot, which keeps browser
// reconnect and multi-client behavior straightforward.
type Center struct {
	mu          sync.RWMutex
	items       map[string]Notification
	order       []string
	subscribers map[int]func([]Notification)
	nextSub     int
}

func New() *Center {
	return &Center{items: make(map[string]Notification), subscribers: make(map[int]func([]Notification))}
}

func (c *Center) List() []Notification {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.listLocked()
}

func (c *Center) Upsert(item Notification) {
	c.mu.Lock()
	if _, exists := c.items[item.ID]; !exists {
		c.order = append(c.order, item.ID)
	}
	c.items[item.ID] = item
	items, callbacks := c.snapshotLocked()
	c.mu.Unlock()
	notify(callbacks, items)
}

func (c *Center) Remove(id string) {
	c.mu.Lock()
	if _, exists := c.items[id]; !exists {
		c.mu.Unlock()
		return
	}
	delete(c.items, id)
	for i, candidate := range c.order {
		if candidate == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	items, callbacks := c.snapshotLocked()
	c.mu.Unlock()
	notify(callbacks, items)
}

func (c *Center) Subscribe(fn func([]Notification)) func() {
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subscribers[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subscribers, id)
		c.mu.Unlock()
	}
}

func (c *Center) listLocked() []Notification {
	items := make([]Notification, 0, len(c.items))
	for _, id := range c.order {
		if item, ok := c.items[id]; ok {
			items = append(items, item)
		}
	}
	return items
}

func (c *Center) snapshotLocked() ([]Notification, []func([]Notification)) {
	items := c.listLocked()
	callbacks := make([]func([]Notification), 0, len(c.subscribers))
	for _, fn := range c.subscribers {
		callbacks = append(callbacks, fn)
	}
	return items, callbacks
}

func notify(callbacks []func([]Notification), items []Notification) {
	for _, fn := range callbacks {
		fn(items)
	}
}
