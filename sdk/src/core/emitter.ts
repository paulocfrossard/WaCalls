/**
 * Minimal typed event emitter — own implementation, zero dependencies.
 * Used by the WaCallsClient facade. Listener errors are isolated (one bad
 * listener cannot break the others or the SSE dispatch loop).
 */

type Listener<T> = (payload: T) => void;

export class Emitter<Events extends { [K in keyof Events]: unknown }> {
  readonly #listeners = new Map<keyof Events, Set<Listener<unknown>>>();

  on<K extends keyof Events>(event: K, cb: Listener<Events[K]>): void {
    let set = this.#listeners.get(event);
    if (!set) {
      set = new Set();
      this.#listeners.set(event, set);
    }
    set.add(cb as Listener<unknown>);
  }

  off<K extends keyof Events>(event: K, cb: Listener<Events[K]>): void {
    this.#listeners.get(event)?.delete(cb as Listener<unknown>);
  }

  emit<K extends keyof Events>(event: K, payload: Events[K]): void {
    const set = this.#listeners.get(event);
    if (!set || set.size === 0) return;
    for (const cb of set) {
      try {
        (cb as Listener<Events[K]>)(payload);
      } catch (err) {
        console.error(`[wacalls-sdk] listener for "${String(event)}" threw`, err);
      }
    }
  }
}
