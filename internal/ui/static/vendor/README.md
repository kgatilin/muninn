# Vendored libraries

Served as they are; the page makes no request outside its own origin.

| file | package | version | source |
|------|---------|---------|--------|
| `d3-force.js` | d3-force | 3.0.0 | https://cdn.jsdelivr.net/npm/d3-force@3.0.0/+esm |
| `d3-dispatch.js` | d3-dispatch | 3.0.1 | https://cdn.jsdelivr.net/npm/d3-dispatch@3.0.1/+esm |
| `d3-quadtree.js` | d3-quadtree | 3.0.1 | https://cdn.jsdelivr.net/npm/d3-quadtree@3.0.1/+esm |
| `d3-timer.js` | d3-timer | 3.0.1 | https://cdn.jsdelivr.net/npm/d3-timer@3.0.1/+esm |

These are jsDelivr's ES module builds, fetched with curl. Two edits were made:
the imports in `d3-force.js` point at the sibling files (`./d3-timer.js`)
instead of `/npm/d3-timer@3.0.1/+esm`, and the trailing `sourceMappingURL`
comment of each file is removed. d3 is ISC licensed, Copyright Mike Bostock.

The d3 license, as the packages ship it:

```
Copyright 2010-2021 Mike Bostock

Permission to use, copy, modify, and/or distribute this software for any purpose
with or without fee is hereby granted, provided that the above copyright notice
and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND
FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS
OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER
TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF
THIS SOFTWARE.
```
