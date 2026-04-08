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
- [x] `s11`: autonomous agents (`idle/claim_task` + task-board polling + auto-claim)
- [x] `s12`: worktree + task isolation (`worktree_*` tools + `.worktrees/index.json` + `events.jsonl`)
- [x] `s_full`: capstone merge of `s01-s11` (all core tools + auto-compact + bg notifications + team orchestration)

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

## Run s11

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s11
```

## Run s12

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s12
```

## Run s_full

```powershell
cd go-pilot
Copy-Item .env.example .env
go run ./cmd/s_full
```
