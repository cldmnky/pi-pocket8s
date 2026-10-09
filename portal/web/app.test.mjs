import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// Every static element dereferenced through the cache must be registered AND
// exist in the embedded page. This catches post-login render failures like the
// missing web-search input cache entries without a browser dependency.
test('all cached UI elements are registered and present', () => {
  const js = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const list = js.match(/var elementIds = \[([\s\S]*?)\];/)[1];
  const ids = new Set([...list.matchAll(/'([^']+)'/g)].map(m => m[1]));
  const references = [...js.matchAll(/elements(?:\['([^']+)'\]|\.([A-Za-z]\w*))/g)];
  for (const [, bracket, dot] of references) {
    const id = bracket || dot;
    assert.ok(ids.has(id), `cache entry missing: ${id}`);
  }
  for (const id of ids) assert.ok(html.includes(`id="${id}"`), `HTML element missing: ${id}`);
});
