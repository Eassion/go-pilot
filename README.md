# go-pilot
一种仿照claude code思想、基于go语言的编码智能体

Go rewrite project for `learn-claude-code`.

## Current status

- [x] Project scaffold
- [x] `s01`: minimal agent loop with one `bash` tool
- [x] OpenAI-compatible provider only
- [x] `s02`: tool dispatch (`bash` + `read_file` + `write_file` + `edit_file`)
- [x] `s03`: todo planning tool + progress reminder (nag after 3 rounds)
- [x] `s04`: subagent delegation tool (`task`)
- [x] `s05`: skill loading (`load_skill` from `skills/**/SKILL.md`)
- [x] `s06`: context compact (micro/auto/manual compression + `.transcripts/`)
- [x] `s07`: persistent task system (`task_create/update/list/get` in `.tasks/`)
- [x] `s08`: background tasks (`background_run/check_background` + notification injection)
- [x] `s09`: agent teams (`spawn_teammate/send_message/read_inbox/broadcast` + `.team/`)
- [x] `s10`: team protocols (`shutdown_request/response` + `plan_approval` with request_id tracking)

## Run s01

```powershell
cd go-pilot
Copy-Item .env.example .env
# Edit .env:
#   MODEL_ID + OPENAI_API_KEY
#   Optional: OPENAI_BASE_URL for compatible providers
go run ./cmd/s01
```

## Run s02

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s02
```

## Run s03

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s03
```

## Run s04

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s04
```

## Run s05

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s05
```

## Run s06

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s06
```

## Run s07

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s07
```

## Run s08

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s08
```

## Run s09

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s09
```

## Run s10

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s10
```
