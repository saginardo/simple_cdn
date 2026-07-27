# Third-Party Notices

## Lucide React 1.25.0

The React console bundles icons from the `lucide-react` package.

ISC License

Copyright (c) 2026 Lucide Icons and Contributors

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

---

Some Lucide icons are derived from the Feather project and retain its MIT notice:

The MIT License (MIT) (for the icons listed above)

Copyright (c) 2013-present Cole Bemis

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

## React UI Runtime

The console uses shadcn CLI/component source 4.13.1, Radix UI 1.6.2, and Recharts 3.8.0. These projects are distributed under the MIT License. shadcn components are checked into `frontend/src/components/ui` so local adaptations remain reviewable.

## Geist Variable Font

The console self-hosts Geist through `@fontsource-variable/geist` 5.2.9. The font is distributed under the SIL Open Font License 1.1.

## Managed Nginx Bundle

The edge bundle contains Nginx 1.30.4 (2-clause BSD-like license), ngx_devel_kit 0.3.4 (3-clause BSD license), lua-nginx-module 0.10.29 (2-clause BSD license), lua-resty-core 0.1.32 (2-clause BSD license), lua-resty-lrucache 0.15 (2-clause BSD license), and OpenResty LuaJIT 2.1-20260724 (MIT license, including the Lua 5.1/5.2 notice).

The complete upstream copyright and license text for every bundled component is installed with the binary under `/opt/cdn-edge/nginx/licenses` and is also present inside `cdn-nginx-linux-amd64.tar.gz`.
