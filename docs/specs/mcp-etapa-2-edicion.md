# Spec: MCP etapa 2 — edición de documentos

| | |
|---|---|
| Estado | Borrador |
| Fecha | 2026-10-22 |
| Depende de | Etapa 1, commit `20c3986` (`search_documents`, `list_documents`, `read_document`) |
| Siguiente | Etapa 3: `move_document`, `delete_document`, `restore_version` |

## 1. Contexto

La etapa 1 dejó al agente leer el wiki: buscar, listar y leer documentos con su
`revision`, que es un hash del contenido actual. La etapa 2 le permite modificarlo
con el mínimo de tools y sin pisar cambios ajenos.

Hoy `POST /api/save/{docPath}` sobrescribe el documento completo sin control de
concurrencia. Para un agente eso tiene dos costos:

- cada cambio reenvía el documento entero;
- puede borrar una edición que una persona guardó entre su lectura y su escritura.

## 2. Objetivos y no objetivos

**Objetivos**

1. Editar un fragmento de un documento enviando solo lo que cambia (`edit_document`).
2. Crear un documento, o reescribirlo completo, en una sola llamada (`write_document`).
3. Dejar un comentario en un documento (`add_comment`).
4. Detectar escrituras concurrentes con `revision`, sin bloqueos que duren más que una llamada.
5. Compartir con la API HTTP una única ruta de escritura: resolución, versiones,
   escritura atómica y lock.

**No objetivos**

- Mover, borrar o restaurar documentos (etapa 3).
- Adjuntos, administración, import y backup.
- Control de concurrencia en la API HTTP: el editor web sigue guardando sin `revision` (§9).

## 3. Flujo de trabajo del agente

```
Editar un fragmento
  search_documents / list_documents ──► path
  read_document(path) ──► revision R1 + Markdown
  edit_document(path, R1, edits) ──► revision R2
  add_comment(path, "qué cambió y por qué")      (opcional)
  si la respuesta es "Revision mismatch" ──► volver a read_document

Crear
  list_documents(prefix) ──► elegir ubicación
  write_document(path, content) ──► "Created", revision R1

Reescribir completo
  read_document(path) ──► R1
  write_document(path, content, R1) ──► revision R2
```

Las `instructions` de `initialize` pasan a describir este flujo (§6.3).

## 4. Reglas comunes

### 4.1 Autenticación y autorización (obligatorio)

Todas las tools nuevas usan el mismo mecanismo que las de la etapa 1, sin atajos:

1. **Rol.** Cada tool declara su rol mínimo en `tool.role`. `callTool` aplica
   `requireRole(SessionFromContext(ctx), tool.role)` antes de ejecutarla, y
   `tools/list` usa la misma regla para filtrar. El rol de la sesión ya viene
   limitado por el `role` del token.
2. **Documento.** Toda lectura o escritura de un documento pasa antes por
   `authorizeDocument(ctx, s, path)`, que resuelve la ruta dentro de la raíz y aplica
   `auth.CanAccessDocument`. Al crear, la verificación se hace sobre la **ruta destino**.
3. **Ocultamiento.** "No existe" y "no tienes acceso" devuelven el mismo error
   (`errDocumentUnavailable`), también al escribir.
4. **Orden.** No se toca el disco (ni `MkdirAll`, ni archivos temporales) antes de
   que pasen los pasos 1 y 2.
5. **Auditoría.** Cada escritura queda en el log a nivel `Info` con usuario, token
   y ruta, por ejemplo
   `MCP edit_document /guides/intro by alice (token "agent")`.

### 4.2 Rutas

Las rutas siguen la convención de la etapa 1: `"guides/intro"`, se ignoran las
barras al inicio y al final, y `""` o `"/"` es la home. `write_document` exige una
ruta canónica solo al **crear** (§5.2).

### 4.3 Revisión y concurrencia optimista

- `revision` = los primeros 8 bytes del SHA-256 del contenido de `document.md`, en hex
  (16 caracteres). Es el mismo valor que ya devuelve `read_document`.
- Una escritura sobre un documento existente debe traer la `revision` que el agente
  leyó. Si no coincide con la actual, falla con
  `Revision mismatch: the document changed since you read it; read it again`.
- El error **no** incluye la revisión actual, para obligar al agente a releer el
  documento antes de reintentar.
- La comparación y la escritura ocurren bajo el mismo lock (§4.4), así que entre
  ellas no hay ventana de carrera para las escrituras que pasan por `documents.Save`.

### 4.4 Ruta de escritura compartida: paquete `internal/documents` (nuevo)

Se crea el paquete y se le mueve la resolución que hoy está en `internal/mcp/access.go`:

```go
package documents

// Document is a wiki document resolved inside the wiki root.
type Document struct {
	LogicalPath string // "/" or "/guides/intro", used by the access rules
	Dir         string // holds document.md and the attachments
	VersionsDir string
}

func (d Document) File() string

// Resolve maps a path argument to a contained document ("" or "/" is the home page).
func Resolve(cfg *config.Config, path string) (Document, error)

// Revision identifies a content; read_document returns it and writes verify it.
func Revision(content []byte) string

type SaveOptions struct {
	ExpectedRevision string // when set, must match the current content
	CreateOnly       bool   // fail with ErrExists when the document exists
}

type SaveResult struct {
	Revision  string // revision of the content now on disk
	Created   bool
	Unchanged bool   // the content was identical; nothing was written
	Version   string // timestamp of the stored previous content, "" if none
}

var (
	ErrExists           = errors.New("document exists")
	ErrNotFound         = errors.New("document not found")
	ErrRevisionMismatch = errors.New("revision mismatch")
)

func Save(cfg *config.Config, doc Document, content []byte, opts SaveOptions) (SaveResult, error)
```

Algoritmo de `Save`:

1. Toma el mutex de escritura del paquete. Basta uno global: las escrituras son
   poco frecuentes y el servidor es un solo proceso.
2. Lee el `document.md` actual, si existe.
3. Si `CreateOnly` y el documento existe → `ErrExists`.
4. Si existe y el contenido es idéntico → `Unchanged`, sin escribir ni crear versión.
5. Si `ExpectedRevision != ""`: si el documento no existe → `ErrNotFound`; si la
   revisión no coincide → `ErrRevisionMismatch`.
6. Versión: si existía, `cfg.Wiki.MaxVersions > 0` y el contenido anterior no estaba
   vacío, guarda el anterior en `VersionsDir/<yyyymmddhhmmss>.md` con `O_EXCL` y luego
   ejecuta `utils.CleanupOldVersions` (§4.5).
7. `MkdirAll(doc.Dir)` y escritura atómica: archivo temporal oculto
   (`.document-*.tmp`) en el mismo directorio, luego `rename` sobre `document.md`.
8. Devuelve `SaveResult` con la revisión nueva.

El reloj se inyecta (`var now = time.Now`) para poder probar colisiones de timestamp.

`SaveHandler` (`internal/handlers/editor.go`) pasa a usar `documents.Resolve` y
`documents.Save(cfg, doc, body, SaveOptions{})`. Su respuesta HTTP no cambia. Los
cambios de comportamiento están en §9.

### 4.5 Historial de versiones

Las versiones se nombran por segundo. Hoy, dos guardados en el mismo segundo se pisan
la versión y se pierde el estado intermedio, y una ráfaga de ediciones de un agente
lo provoca fácilmente. La regla nueva: **si `<ts>.md` ya existe, no se sobrescribe.**
Así siempre se conserva el estado previo a la ráfaga. En ese caso
`SaveResult.Version` queda vacío y la respuesta de la tool omite `previous version`.

Con `max_versions: 0` no se guardan versiones: la sobrescritura no se puede deshacer
(ver §11).

### 4.6 Tamaño de la petición

El endpoint `/mcp` limita cada mensaje a 1 MiB (`maxRequestBytes`), con el contenido
escapado en JSON. Hoy, al superarlo se responde `Parse error`, que no explica nada.
Cambio: si el decoder falla con `*http.MaxBytesError`, se responde HTTP `413` con
`{"success": false, "message": "Request too large (limit 1 MiB)"}`. Un JSON-RPC con
`id` no es posible porque el cuerpo no llegó entero.

Las descripciones de `write_document` y `edit_document` mencionan el límite.

### 4.7 Resultados y errores

- Los errores que el agente puede corregir son tool results con `isError: true`
  (`toolError`). Los fallos de E/S son `Internal error` (JSON-RPC) y se registran en el log.
- Las respuestas de éxito son texto compacto en `clave: valor`, en el mismo estilo
  que los metadatos de `read_document`.

## 5. Tools

Orden en `tools/list`: `search_documents`, `list_documents`, `read_document`,
`edit_document`, `write_document`, `add_comment`. Un viewer ve 4 tools y un editor
o admin ve las 6.

### 5.1 `edit_document`

| | |
|---|---|
| Rol mínimo | `editor` |
| Annotations | `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: false`, `openWorldHint: false` |
| Equivale a | `GET /api/source` + `POST /api/save`, con el parche aplicado en el servidor |

**Descripción (para el modelo):** "Replaces exact text in a wiki document. Pass the
revision from read_document; each edit's old_text must match exactly once in the
current content unless replace_all is set. Edits apply in order and all or none are
saved. The previous content is kept in the history. Requests are limited to 1 MiB."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page"},
    "revision": {"type": "string", "description": "Revision returned by read_document"},
    "edits": {
      "type": "array",
      "minItems": 1,
      "maxItems": 50,
      "items": {
        "type": "object",
        "properties": {
          "old_text": {"type": "string", "description": "Exact text to replace, copied from the Markdown block of read_document"},
          "new_text": {"type": "string", "description": "Replacement text; may be empty to delete old_text"},
          "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a single match", "default": false}
        },
        "required": ["old_text", "new_text"]
      }
    }
  },
  "required": ["path", "revision", "edits"]
}
```

**Comportamiento**

1. Valida los argumentos: `revision` no vacía; entre 1 y 50 `edits`; en cada edit,
   `old_text` no vacío y distinto de `new_text`.
2. `authorizeDocument(path)`; si `document.md` no existe → `errDocumentUnavailable`.
3. Lee el contenido. Si `Revision(contenido) != revision` → mismatch, antes de
   aplicar nada.
4. Aplica los edits en memoria y en orden, cada uno sobre el resultado del anterior.
   Sin `replace_all`, `old_text` debe aparecer exactamente 1 vez. La coincidencia es
   byte a byte, sin normalizar finales de línea ni espacios.
5. Si el resultado queda vacío o solo con espacios → error. Se borra con `delete_document` (etapa 3).
6. `documents.Save(cfg, doc, resultado, SaveOptions{ExpectedRevision: revision})`,
   que vuelve a verificar la revisión bajo el lock.

**Resultado**

```
Saved guides/intro (2 edits applied).
revision: 9c1e4b7a2f0d3e55
previous version: 20261022203000
```

**Errores** (texto exacto; `N` es el índice del edit desde 1)

| Caso | Mensaje |
|---|---|
| Falta `revision` | `Invalid arguments: revision is required; read the document first` |
| `edits` vacío o con más de 50 | `Invalid arguments: edits must have between 1 and 50 items` |
| `old_text` vacío | `Edit N: old_text must not be empty` |
| `old_text == new_text` | `Edit N: new_text equals old_text` |
| Sin coincidencias | `Edit N: old_text not found` |
| Varias coincidencias | `Edit N: old_text matches K times; add surrounding text or set replace_all` |
| Documento vacío | `Edits would leave the document empty` |
| Revisión distinta | `Revision mismatch: the document changed since you read it; read it again` |
| Ruta inválida / sin acceso | `Invalid document path` / `Document not found or access denied` |

### 5.2 `write_document`

| | |
|---|---|
| Rol mínimo | `editor` |
| Annotations | `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false` |
| Equivale a | `POST /api/document/create` + `POST /api/save` |

**Descripción:** "Creates a wiki document or replaces its whole content. Without
revision it only creates: it fails if the document exists. To replace an existing
document pass the revision from read_document; prefer edit_document for partial
changes. New paths use letters, digits, '-', '_' and '/'. Start the content with
front matter (---\nlayout: kanban\n---) for a kanban or links page. Requests are
limited to 1 MiB."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Document path, e.g. \"guides/deploy\"; \"/\" for the home page"},
    "content": {"type": "string", "description": "Full Markdown content, including the # title"},
    "revision": {"type": "string", "description": "Revision from read_document; required to replace an existing document"}
  },
  "required": ["path", "content"]
}
```

No hay parámetros `title` ni `type`: el contenido trae el título (H1) y el front matter
de layout. A diferencia de `/api/document/create`, no se generan plantillas.

**Comportamiento**

1. `content` no vacío (ignorando espacios).
2. `authorizeDocument(path)` sobre la ruta destino.
3. Lee el `document.md` actual, si existe.
4. **No existe → crear.**
   - Si se envió `revision` → `errDocumentUnavailable`: el documento leído ya no está.
   - Para rutas distintas de la home: si `utils.SanitizePath(path) != strings.Trim(path, "/")`
     → error con la ruta sugerida.
   - `Save(..., SaveOptions{CreateOnly: true})`. Si mientras tanto alguien lo creó → `ErrExists`.
   - Los directorios intermedios se crean sin `document.md`, igual que hoy en la API.
5. **Existe.**
   - Contenido idéntico → `Unchanged`, haya o no `revision`. Así un reintento
     después de un éxito es idempotente.
   - Sin `revision` → error "Document exists".
   - Con `revision` → `Save(..., SaveOptions{ExpectedRevision: revision})`.

**Resultado**

```
Created guides/deploy.
revision: 4f0a9d1c3b2e7a66
```
```
Saved guides/intro.
revision: 9c1e4b7a2f0d3e55
previous version: 20261022203000
```
```
Unchanged guides/intro: the content is identical.
revision: 9c1e4b7a2f0d3e55
```

**Errores**

| Caso | Mensaje |
|---|---|
| `content` vacío | `Invalid arguments: content must not be empty` |
| Ruta no canónica al crear | `Invalid path for a new document; use "Guides/My-Doc"` (la sugerencia sale de `SanitizePath`) |
| Existe y no hay `revision` | `Document exists; pass the revision from read_document to replace it` |
| Revisión distinta | `Revision mismatch: the document changed since you read it; read it again` |
| Se envió `revision` pero el documento no existe | `Document not found or access denied` |
| Ruta inválida / sin acceso | `Invalid document path` / `Document not found or access denied` |

### 5.3 `add_comment`

| | |
|---|---|
| Rol mínimo | `viewer` (igual que `/api/comments/add`) |
| Annotations | `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: false`, `openWorldHint: false` |
| Equivale a | `POST /api/comments/add/{docPath}` |

**Descripción:** "Adds a Markdown comment to a wiki document as the token owner.
Use it to explain a change or to ask the document owners a question."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page"},
    "content": {"type": "string", "description": "Comment in Markdown"}
  },
  "required": ["path", "content"]
}
```

**Comportamiento** (mismo orden de verificaciones que `AddCommentHandler`)

1. Si `cfg.Wiki.DisableComments` → error.
2. `authorizeDocument(path)`; si `document.md` no existe → `errDocumentUnavailable`.
3. Si `comments.AreCommentsAllowed(contenido)` es falso (marcador `<!-- no comments -->`) → error.
4. `content` no vacío.
5. `comments.AddComment(<root>/comments, doc.LogicalPath, content, session.Username)`.

**Cambio en `internal/comments`:** el ID es `<yyyymmddhhmmss>_<usuario>.md`, así que
dos comentarios del mismo usuario en el mismo segundo hoy se sobrescriben. Para
evitarlo, `AddComment` crea el archivo con `O_CREATE|O_EXCL` y devuelve
`ErrCommentExists` si ya existe. El reloj se inyecta para las pruebas.

**Resultado:** `Comment added to guides/intro.`

**Errores**

| Caso | Mensaje |
|---|---|
| Comentarios desactivados | `Comments are disabled in this wiki` |
| Marcador en el documento | `Comments are not allowed for this document` |
| `content` vacío | `Invalid arguments: content must not be empty` |
| Mismo segundo | `You already commented on this document this second; retry` |
| Ruta inválida / sin acceso | `Invalid document path` / `Document not found or access denied` |

## 6. Cambios en el código

### 6.1 Archivos

| Archivo | Cambio |
|---|---|
| `internal/documents/documents.go` (nuevo) | `Document`, `Resolve`, `Revision`, `Save`, errores y mutex (§4.4) |
| `internal/documents/documents_test.go` (nuevo) | Pruebas de `Save` (§8.1) |
| `internal/mcp/access.go` | `authorizeDocument` devuelve `documents.Document`; se elimina `resolveDocument` |
| `internal/mcp/read_document.go` | Usa `documents.Revision`; se elimina `contentRevision` |
| `internal/mcp/edit_document.go` (nuevo) | Tool §5.1 |
| `internal/mcp/write_document.go` (nuevo) | Tool §5.2 |
| `internal/mcp/add_comment.go` (nuevo) | Tool §5.3 |
| `internal/mcp/tools.go` | Registra las tools; agrega `writeAnnotations(idempotent bool)` |
| `internal/mcp/server.go` | `instructions` (§6.3) y `413` (§4.6) |
| `internal/handlers/editor.go` | `SaveHandler` usa `documents.Save` |
| `internal/comments/comments.go` | `AddComment` con `O_EXCL` y `ErrCommentExists` |
| `internal/handlers/comments.go` | `ErrCommentExists` → `409 Conflict` |

Dependencias: `handlers` → `documents` y `mcp` → `documents`. `documents` no importa
ni `handlers` ni `mcp`.

### 6.2 Annotations

`readOnlyAnnotations` se mantiene igual. Para las tools de escritura se agrega:

```go
func writeAnnotations(idempotent bool) map[string]interface{} {
	return map[string]interface{}{
		"readOnlyHint":    false,
		"destructiveHint": false, // the previous content stays in the history
		"idempotentHint":  idempotent,
		"openWorldHint":   false,
	}
}
```

### 6.3 Instructions

```
Wiki-Go documents are Markdown pages addressed by path, for example "guides/intro";
"/" is the home page. Find documents with search_documents or browse them with
list_documents, then read one with read_document. To change a document pass the
revision from read_document to edit_document (exact replacements) or write_document
(whole content); a revision mismatch means someone else changed it, so read it again.
write_document without revision creates a new document. Use add_comment to explain
changes. Results only include the documents your role and groups may access.
```

Las tools que el rol de la sesión no ve en `tools/list` siguen nombradas aquí. Es
aceptable: llamarlas devuelve `Insufficient role`.

## 7. Seguridad

- Ninguna tool escribe fuera de `documents/`, `pages/home/`, `versions/` y
  `comments/`. Todas las rutas pasan por `documents.Resolve`
  (`ResolveRelativeDocumentPath`), que rechaza `..`, rutas absolutas, `%xx` y bytes nulos.
- Al crear, la ruta canónica (`SanitizePath`) no puede tener `.`, así que tampoco
  segmentos ocultos ni `document.md` como directorio.
- Antes de verificar el acceso no se crea nada en disco: un intento sobre una
  sección restringida no deja directorios vacíos.
- Un token con `role: viewer` no ve ni puede llamar `edit_document` ni
  `write_document`, aunque su dueño sea editor.
- El contenido no se renderiza ni se sanea al escribir. Se sanea al mostrarlo,
  igual que hoy con `/api/save`.

## 8. Pruebas (criterios de aceptación)

### 8.1 `internal/documents`

- `Save` crea, sobrescribe y guarda la versión anterior con el contenido previo exacto.
- `ExpectedRevision` obsoleta → `ErrRevisionMismatch` y el archivo queda intacto.
- `CreateOnly` sobre un documento existente → `ErrExists`.
- Contenido idéntico → `Unchanged`, sin versión nueva.
- Dos `Save` en el mismo segundo (reloj inyectado) → se conserva la primera versión;
  la segunda devuelve `Version == ""`.
- `max_versions: 0` → no se crean versiones.
- No quedan archivos `.document-*.tmp` después de un `Save` exitoso ni de uno fallido.
- `Revision` devuelve los mismos valores que `contentRevision` de la etapa 1.

### 8.2 Tools

**`edit_document`**
- Un viewer no la ve en `tools/list` y llamarla da `Insufficient role: editor access required`.
- El editor sin grupo `finance` sobre `finance/report` recibe `errDocumentUnavailable`;
  con el grupo puede editar.
- Un edit simple deja el contenido esperado, devuelve una revisión que coincide con
  la de `read_document` y crea la versión.
- Con varios edits, si falla el segundo no se escribe nada.
- Coincidencia ambigua → error con el conteo; con `replace_all` se reemplazan todas.
- `old_text` vacío, `new_text == old_text`, texto no encontrado y documento vacío dan
  los mensajes de §5.1.
- Una revisión obsoleta da mismatch y el archivo queda intacto.
- Rutas con `..` → `Invalid document path`.

**`write_document`**
- Crear: contenido exacto en disco, sin versión, y el documento aparece en `list_documents`.
- `Guides/My Doc` → error con la sugerencia `Guides/My-Doc`.
- Crear en una sección restringida sin grupo → `errDocumentUnavailable`, sin directorios creados.
- Existente sin revisión → "Document exists"; con revisión obsoleta → mismatch; con la
  correcta → guarda y crea versión.
- Contenido idéntico → `Unchanged` sin versión, con o sin revisión.
- La home `/` se reescribe con su revisión.
- `revision` sobre una ruta inexistente → `errDocumentUnavailable`.

**`add_comment`**
- Un viewer comenta; el autor es el dueño del token y el comentario aparece en
  `read_document` con `include: ["comments"]`.
- `disable_comments`, el marcador `<!-- no comments -->`, un contenido vacío y un
  documento restringido o inexistente dan los mensajes de §5.3.
- Dos comentarios en el mismo segundo (reloj inyectado) → el segundo falla y el
  primero queda intacto.

### 8.3 Servidor y regresiones

- `tools/list` para un editor devuelve las 6 tools en el orden de §5; para un viewer, 4.
- Un cuerpo mayor a 1 MiB → `413` con el mensaje de §4.6.
- `SaveHandler` sigue guardando y creando versiones; contenido idéntico ya no crea versión.
- `AddCommentHandler` responde `409` en la colisión del mismo segundo.
- Las pruebas de la etapa 1 siguen pasando sin cambios.

## 9. Cambios visibles en la API HTTP

| Endpoint | Antes | Después |
|---|---|---|
| `POST /api/save/{docPath}` | Guardar contenido idéntico creaba una versión duplicada | No escribe ni crea versión |
| `POST /api/save/{docPath}` | Dos guardados en el mismo segundo pisaban la versión | Se conserva la primera versión de ese segundo |
| `POST /api/save/{docPath}` | `os.WriteFile` directo | Escritura atómica (temporal + rename) |
| `POST /api/comments/add/{docPath}` | Dos comentarios en el mismo segundo se sobrescribían | `409 Conflict`; el primero se conserva |

Limitación conocida: el editor web guarda sin `revision`. Si una persona guarda
encima del cambio de un agente, la versión del agente queda en el historial (cuando
`max_versions > 0`), pero no se avisa a nadie. Agregar `If-Match` con la revisión a
`/api/save` queda para una etapa posterior.

## 10. Documentación

- `docs/openapi.yaml`: agregar las tres tools a la sección `/mcp`, el `413`, los nuevos
  comportamientos de `/api/save` y el `409` de `/api/comments/add`.
- `rest-api-examples.http`: ejemplos de `edit_document`, `write_document` y
  `add_comment`. El token de ejemplo necesita `role: editor` para las dos primeras.

## 11. Preguntas abiertas

1. **`destructiveHint` con `max_versions: 0`.** Sin historial, sobrescribir no se puede
   deshacer. ¿Se calcula la anotación según la configuración, o basta con avisarlo en
   la descripción?
2. **Autoría de los comentarios de agentes.** El autor es el dueño del token. ¿Hay que
   distinguir los comentarios hechos con un token? Requiere cambiar el formato del ID
   (`<ts>_<usuario>.md`) o agregar metadatos al comentario.
3. **Límite de 1 MiB.** ¿Se sube para documentos grandes, o se mantiene y se dirige al
   agente a `edit_document`?
4. **Lock entre procesos.** El mutex es del proceso. Si algún día corren varias
   instancias sobre el mismo directorio de datos, haría falta un lock de archivo.
