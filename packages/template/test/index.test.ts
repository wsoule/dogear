import { describe, expect, test } from 'bun:test';

import { greet } from '../src/index';

describe('greet', () => {
  test('greets by name', () => {
    expect(greet('world')).toBe('Hello, world!');
  });
});
