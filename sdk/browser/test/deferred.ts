/**
 * Test seam for a promise the test settles by hand.
 *
 * `Promise.withResolvers` would be the obvious spelling, but this package
 * compiles with `lib: ES2018` (the bundle ships to browsers we do not get to
 * choose), so the constructor form is the only one that typechecks.
 */
export interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T | PromiseLike<T>) => void;
}

export function deferred<T>(): Deferred<T> {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
