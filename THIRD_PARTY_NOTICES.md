# Third-party notices

This project adapts CodeArts protocol, credential, pool, and gateway code from:

- HITZY2002/codearts2api, MIT, commit `8d1e43d6f31279ac20cf94b92eff20bdba784b76`.
  The original license is preserved in LICENSE.

The management panel follows the structure and visual design of workbuddy2api-panel.
Request metrics and log components are adapted from that project:

- linguo2625469/workbuddy2api-panel, MIT, local reference commit `73a7560`.
- Original project: Sliverkiss/workbuddy2api.

CodeArts protocol details were also checked against:

- iJetLi/deepseek-harness-codearts, commit `2bf76ce66f1214f18ee3202238fb0f2d02cc791f`.
- Johnsheng1/codearts2api, commit `d813ca0456f23cd73bb3bbee16627bc2a40036f9`.

## Protocol conversion package (LGPL-3.0)

`internal/apicompat/` is a verbatim port of a third-party package and is licensed
**LGPL-3.0**, NOT MIT. It translates between the Anthropic Messages protocol
(Claude clients) / OpenAI Responses protocol and OpenAI chat completions.

- Original source: `https://github.com/Wei-Shaw/sub2api`
  (`backend/internal/pkg/apicompat/`, commit `d89e533424ea58cb286c05e957f0cc0e54949ae6`),
  relicensed to LGPL-3.0 by commit `23def40bc`.
- Ported via `workbuddy2api` (MIT) which copied it verbatim on 2026-09-12.
- Copied into this repository verbatim on 2026-11-04.
- The LGPL-3.0 text is in `internal/apicompat/LICENSE`; provenance in
  `internal/apicompat/PROVENANCE.md`.

This package depends only on the Go standard library.

## WorkBuddy2API Panel license

MIT License

Copyright (c) 2026 Sliverkiss (original project: https://github.com/Sliverkiss/workbuddy2api)
Copyright (c) 2026 linguo2625469 (this fork: https://github.com/linguo2625469/workbuddy2api-panel)

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
