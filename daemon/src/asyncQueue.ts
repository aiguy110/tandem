// A tiny push-driven AsyncIterable so adapters can expose `events` per the spec
// while internally just calling push()/close().

export class AsyncQueue<T> implements AsyncIterable<T> {
  private items: T[] = [];
  private waiters: ((r: IteratorResult<T>) => void)[] = [];
  private closed = false;

  push(v: T): void {
    const w = this.waiters.shift();
    if (w) w({ value: v, done: false });
    else this.items.push(v);
  }

  close(): void {
    this.closed = true;
    let w;
    while ((w = this.waiters.shift())) w({ value: undefined as unknown as T, done: true });
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: (): Promise<IteratorResult<T>> => {
        if (this.items.length) return Promise.resolve({ value: this.items.shift() as T, done: false });
        if (this.closed) return Promise.resolve({ value: undefined as unknown as T, done: true });
        return new Promise((res) => this.waiters.push(res));
      },
    };
  }
}
