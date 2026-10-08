# Vendored: pi-web-search

`vendor/` is the source of the npm package **pi-web-search 1.7.0**
(<https://github.com/ttttmr/pi-web-search>, MIT as declared in its `package.json`;
the upstream repository ships no `LICENSE` file), copied unmodified except that
the package's `src/index.ts` was **not** vendored: that file is the Pi extension
entry point (Pi's `registerTool` API plus pi-tui rendering), which Pi Pocket
cannot load. Everything else under `src/` is here, with the same relative
imports.

The adapter that makes it a Pi Pocket drop-in lives one level up:

- `../web-search.ts` — the module: one `web_search` tool, Pi Durable's tool API.
- `../model.ts` — the model to search with, and the small context shim the
  vendored code needs (`model`, `modelRegistry`, `sessionManager.getSessionId`).

Keep vendored files unmodified so they can be diffed against a later release.
Put local changes in the adapter instead. Note that Pi Pocket does not reload
helpers when they change: editing anything here needs a restart, not just a
re-save of the module.

## License

MIT License

Copyright (c) the pi-web-search authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
