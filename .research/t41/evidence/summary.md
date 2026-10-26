# Part B 结果汇总

- 开始: 2026-10-20T10:31:50+08:00
- 结束: 2026-10-20T10:41:47+08:00
- Lead pin 指纹: 5f7158de4ace768ed0917a5d15624f4f3805722a03b8eae71caf4c2ae2203e1e（期望 5f7158de4ace768ed0917a5d15624f4f3805722a03b8eae71caf4c2ae2203e1e）
- pin 稳定: ok

| 阶段 | 结果 |
|---|---|
| 0a-pin-file | ok |
| 0b-pin-check | ok |
| 0c-frozen-copy | ok |
| 0d-frozen==worktree | ok |
| 0e-build-old(HEAD) | ok |
| 0f-build-new(frozen) | ok |
| 0g-build-nofilter | ok |
| 1-fixtures | ok |
| 2a-api-diff-isolation | ok |
| 2b-api-diff-e2e | ok |
| 3-order | ok |
| 4-restart | ok |
| 5-mutations | ok |
| 6a-go-test | known-red |
| 6b-baseline-fails-expected | ok |
| 6c-go-test-race | skipped |
| 7-wording | ok |
| 8-pin-stable | ok |

## ❌ 未通过
- 6a-go-test
