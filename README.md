# go-pilot
一种仿照claude code思想、基于go语言的编码智能体

Go rewrite project for `learn-claude-code`.

## Current status

- [x] Project scaffold
- [x] `s01`: minimal agent loop with one `bash` tool
- [x] OpenAI-compatible provider only

## Run s01

```powershell
cd go-pilot
Copy-Item .env.example .env
# Edit .env:
#   MODEL_ID + OPENAI_API_KEY
#   Optional: OPENAI_BASE_URL for compatible providers
go run ./cmd/s01
```
