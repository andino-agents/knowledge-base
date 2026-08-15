# andino-kb

RAG headless en Go: indexa fuentes (vault de Obsidian, repos git, S3) y las sirve por REST y
MCP. Un solo binario estático.

## Comandos

```bash
go build ./...      # Go >= 1.26
go test ./...       # no necesita nada externo, SQLite corre en proceso
make build          # binario en bin/andino-kb
gofmt -l . && go vet ./...
```

CI rechaza código sin formatear: corre `gofmt` y `go vet` antes de dar nada por terminado.

## Innegociables

- **`CGO_ENABLED=0`.** Una dependencia que rompa la compilación cruzada en Go puro no entra.
- **Los modos de fallo son la especificación.** Sin fallbacks silenciosos: un error tiene que
  salir a la superficie, jamás degradar los datos. En concreto: un fallo del endpoint de
  embeddings aborta ese documento y conserva la versión anterior, no existen vectores dummy.
- **Los cambios de recuperación se miden.** Un cambio que toque la calidad de búsqueda va con
  números de `andino-kb eval` (recall@k, MRR) sobre un corpus real, o con la razón de por qué
  no se puede medir.
- Comportamiento nuevo, test nuevo. Los proveedores de almacenamiento pasan el harness de
  conformidad de `internal/store/storetest`.

## Convenciones

- Commits: **conventional commits en inglés** (`fix(serve): ...`, `feat(index): ...`), con el
  porqué en el cuerpo, en imperativo. Referencia issues con `Refs #N` o `Closes #N`.
- Una preocupación por PR.
- Comentarios de código en inglés.

## Gotchas

- El endpoint de chat (contextual retrieval) y el de embeddings **cargan por separado** en un
  router de llama.cpp. Por eso `serve` y `index` esperan a los dos con `WaitReady` antes de
  indexar. No quites esa espera.
- El probe de `WaitReady` ignora el cuerpo de la respuesta a propósito: un modelo thinking-first
  gasta su único token en razonar y devuelve contenido vacío, que es un error real al indexar
  pero **demuestra igual que el modelo está cargado**.
- Un router en su tope de modelos contesta `400 "model is not loaded"`, que `Chat.Complete`
  **no reintenta** (solo 5xx y 429). Por eso el sync inicial reintenta por encima.
- `/healthz` es liveness y devuelve `ok` aunque el índice esté muerto. El estado real es
  **`/readyz`**.
