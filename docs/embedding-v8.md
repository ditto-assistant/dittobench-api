# Bench v8 hosted embedding route

Bench v8 has a dark, version-selected route for hosted embeddings. It is not an
activated benchmark version and it cannot change the embedding contract used by
v2-v7.

The miner-facing API remains Ollama-compatible: model `embeddinggemma` at
`POST /api/embed`, returning exactly 768 floats per ordered input. The trusted
`embedding-proxy` image translates that operation to the locked OpenRouter
profile in `embedding-v8-profile.json`. The provider key exists only in that
proxy process.

The scorer selects `DITTOBENCH_V8_EMBEDDING_UPSTREAM_URL` only for a session
whose bound benchmark version is exactly 8. The URL is disabled unless
`DITTOBENCH_V8_EMBEDDING_PROFILE_REVISION` exactly matches the reviewed profile.
Historical sessions always use `DITTOBENCH_EMBEDDING_UPSTREAM_URL`, preserving
the released local `embeddinggemma` contract.

Build the trusted proxy independently from the scorer:

```bash
docker build --target embedding-proxy -t dittobench-embedding-proxy .
docker run --read-only --tmpfs /tmp:rw,noexec,nosuid,size=2g \
  -e OPENROUTER_API_KEY \
  -e OPENROUTER_EMBED_PROFILE=v8 \
  dittobench-embedding-proxy
```

Do not enable the route merely because the transport works. The starter kit's
current fusion MLP was trained in the local `embeddinggemma` vector space. The
candidate features must be regenerated and the MLP retrained before the v8
multi-seed campaign. The activation gates are machine-readable in the profile
manifest; the paired starter-kit PR records the initial retrieval measurement.

`embedding-v8-smoke.json` records a live test of the built image through the
unchanged miner-facing operation. OpenRouter returned one 768-dimensional
vector with the exact v8 profile identity and zero proxy failures. This proves
the transport and credential boundary, not the outstanding MLP or benchmark
calibration gates.
