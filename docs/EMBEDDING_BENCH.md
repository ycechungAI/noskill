# Embedding model baseline (2026-10-25)

Seven embedding models run through `noskill models bench <model>` (the embedding suite in
`internal/app/benchother.go`, FR-B4) on an Apple M4. Everything except `nomic-embed-text` (the model
noskill keeps) was pulled for the run and removed again; only these numbers stay. The raw results
are in [benchmarks/embeddings-2026-10-25.json](benchmarks/embeddings-2026-10-25.json).

| Model | Skill pick (top 1) | MRR | Other languages | Long paragraphs | Paraphrases | Per text | Cold load | Memory |
|---|---|---|---|---|---|---|---|---|
| embeddinggemma | 0.88 | 0.94 | 1.00 | 1.00 | 1.00 | 15 ms | 1.1 s | 0.69 GB |
| mxbai-embed-large | 0.88 | 0.94 | 0.67 | 1.00 | 1.00 | 88 ms | 0.8 s | 0.63 GB |
| nomic-embed-text | 0.88 | 0.94 | 0.50 | 1.00 | 1.00 | 16 ms | 0.3 s | 0.34 GB |
| granite-embedding:278m | 0.50 | 0.69 | 1.00 | 1.00 | 1.00 | 17 ms | 1.3 s | 0.19 GB |
| snowflake-arctic-embed:335m | 0.88 | 0.91 | 0.33 | 1.00 | 1.00 | 28 ms | 0.6 s | 0.63 GB |
| bge-m3 | 0.38 | 0.64 | 1.00 | 1.00 | 1.00 | 24 ms | 1.6 s | 0.69 GB |
| all-minilm | 0.88 | 0.92 | 0.17 | 1.00 | 1.00 | 10 ms | 0.6 s | 0.03 GB |

What the columns test (8 skill descriptions, requests that share few words with them):

- **Skill pick / MRR:** is the right skill description the closest to the request (and how high does it rank when it isn't)?
- **Other languages:** the same kind of request in Spanish, French, Chinese and Japanese.
- **Long paragraphs:** the right 50-word paragraph out of three.
- **Paraphrases:** does a sentence sit nearer its paraphrase than an unrelated sentence?

Read this as one small suite, not a verdict. Twelve to eighteen questions per column means one
miss moves a score by 0.06 to 0.17. `bge-m3` and `granite-embedding:278m` score low on the skill pick
while leading the other-language column, which may be this suite's wording rather than the models:
those models are made for longer passages and queries with an instruction prefix that the Ollama
`/api/embed` call doesn't add. Worth a second look before swapping `embed_model`, which stays
`nomic-embed-text` (fast to load, 0.3 GB, solid on everything but other languages).
