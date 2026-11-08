# Spec: MCP etapa 3 — estructura e historial

| | |
|---|---|
| Estado | Borrador |
| Fecha | 2026-10-22 |
| Depende de | Etapa 2 ([mcp-etapa-2-edicion.md](mcp-etapa-2-edicion.md)): paquete `internal/documents`, `Save`, mutex de escritura y `revision` |
| Cierra | El plan de tres etapas. Lo que queda está en §12 |

## 1. Contexto

Con las etapas 1 y 2 el agente puede leer, editar, crear y comentar documentos. Le
falta reorganizar el wiki y deshacer cambios. La API ya tiene estas operaciones, pero
tres de sus problemas pesan más cuando quien opera es un agente:

1. **Mover y borrar actúan sobre todo el subárbol, pero solo verifican la raíz.**
   `MoveDocumentHandler` y `DeleteDocumentHandler` revisan el acceso a la ruta pedida
   y luego hacen `os.Rename` u `os.RemoveAll` del directorio completo. Si un
   editor puede ver `guides` pero no `guides/secret`, igual puede moverlo o borrarlo
   con su padre.
2. **Mover puede cambiar en silencio quién ve un documento.** Las reglas de acceso se
   aplican por ruta (`/finance/**`). Si se mueve `finance/report` a `archive/report`,
   el documento queda bajo otra regla, quizá pública, sin aviso.
3. **El borrado es irreversible y la restauración pisa versiones.** `delete` elimina
   también el historial y los comentarios. Restaurar guarda la versión actual con el
   mismo esquema por segundo que `/api/save`, y comparte su colisión.

Además, mover o borrar deja rotos los enlaces de otros documentos, y el índice de
wikilinks por nombre (`goldext.cachedSlugIndex`) solo se invalida cuando cambia el
mtime de la raíz `documents/`. Un rename anidado (`a/b` → `a/c`) no lo invalida.

## 2. Objetivos y no objetivos

**Objetivos**

1. Mover o renombrar un documento con su subárbol, versiones y comentarios (`move_document`).
2. Borrar un documento con una confirmación explícita y proporcional al daño (`delete_document`).
3. Restaurar una versión sin pasar el contenido por el agente (`restore_version`).
4. Que ninguna operación de estructura afecte documentos que el usuario no puede ver.
5. Que el agente sepa qué enlaces quedaron rotos, para corregirlos con `edit_document`.
6. Que la API HTTP use la misma implementación y así herede las correcciones de seguridad.

**No objetivos**

- Papelera o borrado recuperable (§12, pregunta 1).
- Reescribir automáticamente los enlaces de otros documentos (§12, pregunta 2).
- Redirecciones desde rutas antiguas.
- Adjuntos y administración.

## 3. Flujo de trabajo del agente

```
Reorganizar
  list_documents(prefix) ──► elegir origen y destino
  move_document(path, new_path) ──► "Moved", lista de documentos que enlazan a la ruta vieja
  si "Moving changes the access rules…" ──► confirmar con el usuario
                                         ──► move_document(…, allow_access_change: true)
  por cada referencia: read_document ──► edit_document (actualizar el enlace)

Deshacer un cambio
  read_document(path, include: ["history"]) ──► revision R1 + versiones
  read_document(path, version: T)            ──► revisar el contenido viejo (opcional)
  restore_version(path, T, R1)               ──► revision R2

Borrar
  read_document(path) ──► revision R1
  delete_document(path, R1)                  ──► falla si hay sub-documentos
  list_documents(prefix: path, depth: 0)     ──► revisar qué se borraría
  delete_document(path, R1, recursive: true) ──► "Deleted …", referencias rotas
```

## 4. Reglas comunes

### 4.1 Autenticación y autorización (obligatorio)

Se mantiene el mecanismo de las etapas anteriores:

1. **Rol.** Las tres tools declaran `role: editor` y `callTool` aplica `requireRole`
   antes de ejecutarlas. Un token `role: viewer` no las ve ni puede llamarlas.
2. **Documento.** Toda ruta de origen o destino pasa por `authorizeDocument` antes de
   tocar el disco.
3. **Subárbol (nuevo).** Mover y borrar requieren acceso a **cada directorio del
   subárbol**, con `auth.CanAccessDocument` aplicado a su ruta lógica. Al mover, se
   exige acceso en la ruta vieja y en la nueva. La verificación corre bajo el mutex de
   escritura (§4.3), así que entre la verificación y la operación nadie puede agregar
   al subárbol un documento restringido.
4. **Reglas de acceso al mover (nuevo).** Si algún documento del subárbol queda bajo
   otra regla después de mover (§5.1, paso 6), la operación se rechaza salvo que se
   pase `allow_access_change: true`. Aplica también a admins: protege contra errores
   del agente, no contra falta de permisos.
5. **Ocultamiento.** "No existe" y "no tienes acceso" en el origen devuelven
   `errDocumentUnavailable`. La excepción es la verificación del subárbol: su error
   revela que hay documentos no visibles, pero no sus nombres ni cuántos son (§8).
6. **Auditoría.** Log `Info` con usuario, token, operación, rutas y número de documentos afectados.

### 4.2 Rutas

Igual que en las etapas anteriores: `"guides/intro"`, se ignoran las barras de los
extremos, y `""` o `"/"` es la home. La home no se puede mover, borrar ni usar como
destino. `new_path` debe ser canónica (`utils.SanitizePath(p) == strings.Trim(p, "/")`),
igual que una ruta nueva en `write_document`.

### 4.3 Extensión del paquete `internal/documents`

```go
// Moved pairs a directory of a moved subtree with its new location.
type Moved struct {
	From, To string // logical paths, e.g. "/guides/intro" → "/archive/intro"
}

// Move renames src to dst together with its versions and comments. It holds
// the write lock; check runs under it with every directory of the subtree
// (src first) and can veto the move by returning an error.
func Move(cfg *config.Config, src, dst Document, check func([]Moved) error) error

type DeleteOptions struct {
	ExpectedRevision string // when set, must match src's document.md
	Recursive        bool   // allow deleting sub-documents
}

type DeleteResult struct {
	Documents   []string // logical paths of the deleted documents, doc first
	Attachments int      // files other than document.md that were removed
	Warnings    []string // leftovers that could not be removed
}

// Delete removes doc with its subtree, versions and comments. check runs under
// the write lock with every directory of the subtree and can veto it.
func Delete(cfg *config.Config, doc Document, opts DeleteOptions, check func([]string) error) (DeleteResult, error)

// References returns the documents, other than those under exclude, whose
// Markdown links to logicalPath (§4.5).
func References(cfg *config.Config, logicalPath, exclude string) ([]string, error)

var (
	ErrTargetExists = errors.New("target exists")
	ErrInsideItself = errors.New("target is inside the source")
)

// ChildrenError reports sub-documents when Recursive is false.
type ChildrenError struct{ Count int }
```

Además, `Save`, `Move` y `Delete` usan el mismo mutex de escritura de la etapa 2.

**Algoritmo de `Move`**

1. Toma el mutex.
2. Verifica que `src.Dir` exista y que `dst` no esté dentro de `src`.
3. Verifica que `dst.Dir` no exista, o que sea un directorio vacío. Se permite un
   rename que solo cambia mayúsculas, como hoy en la API.
4. Recorre el subárbol de `src.Dir`: cada directorio, oculto o no, produce un `Moved`.
   Llama a `check`.
5. `MkdirAll(filepath.Dir(dst.Dir))` y luego `os.Rename` del directorio del documento.
6. Si existen, `os.Rename` del directorio de versiones (`VersionsDir`) y del de
   comentarios (`<root>/comments/<ruta>`). Si alguno falla, **revierte** los renames
   ya hechos en orden inverso y devuelve el error. Hoy la API solo registra un `Warn`
   y deja el historial desacoplado del documento.
7. Toca el mtime de `documents/` (§4.4).

**Algoritmo de `Delete`**

1. Toma el mutex y verifica que `doc.Dir` exista.
2. Si `ExpectedRevision != ""`, compara con `Revision(document.md)` → `ErrRevisionMismatch`.
3. Recorre el subárbol; si hay sub-documentos y `!Recursive` → `*ChildrenError`.
4. Llama a `check` con los directorios del subárbol.
5. `os.RemoveAll` del directorio del documento. Después, del de versiones y del de
   comentarios. Lo que no se pueda borrar va a `Warnings`: no aborta, porque el
   documento ya no está, pero se informa en vez de quedar solo en el log.
6. Toca el mtime de `documents/` (§4.4).

Los directorios de versiones y comentarios de un documento incluyen los de sus
sub-documentos, porque se anidan igual. Por eso un solo rename o borrado los cubre.

### 4.4 Invalidación del índice de wikilinks

`goldext.cachedSlugIndex` reconstruye el índice `nombre → ruta` cuando cambia el mtime
de `documents/`. Después de un `Move`, de un `Delete` y de un `Save` con `Created`, el
paquete ejecuta `os.Chtimes(documentsRoot, now, now)`. Así `[[nombre]]` resuelve a la
ubicación nueva sin reiniciar el servidor. Si la etapa 2 ya está implementada, el
cambio en `Save` se agrega en esta etapa.

### 4.5 Referencias

`References` recorre los `document.md` del wiki (incluida la home) y busca, fuera de
los bloques de código, enlaces a la ruta vieja seguidos de `)`, `/`, `#`, `|`, `]`,
espacio o comillas:

| Forma | Ejemplo para `guides/old` |
|---|---|
| Enlace Markdown absoluto | `](/guides/old)`, `](/guides/old#setup)`, `](/guides/old/child)` |
| Wikilink con ruta | `[[guides/old]]`, `[[guides/old\|Old]]` |
| Adjunto | `/api/files/guides/old/chart.png` |
| Wikilink por nombre, solo si cambia el último segmento | `[[old]]` |

Excluye los documentos del subárbol movido o borrado. La tool filtra el resultado con
`CanAccessDocument`, para no revelar documentos restringidos, y lo corta en 20
entradas. No detecta enlaces relativos (`../old`): es una limitación documentada.

### 4.6 Resultados y errores

Las mismas convenciones de la etapa 2: los errores que el agente puede corregir son
`toolError` con `isError: true`, los de E/S son `Internal error` y quedan en el log, y
las respuestas de éxito son texto `clave: valor`.

## 5. Tools

Orden final en `tools/list`: `search_documents`, `list_documents`, `read_document`,
`edit_document`, `write_document`, `add_comment`, `move_document`, `restore_version`,
`delete_document`. Un viewer ve 4 tools y un editor o admin ve las 9; la única
destructiva queda al final.

### 5.1 `move_document`

| | |
|---|---|
| Rol mínimo | `editor` |
| Annotations | `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: false`, `openWorldHint: false` |
| Equivale a | `POST /api/document/move` |

**Descripción:** "Moves or renames a wiki document together with its sub-documents,
attachments, history and comments. new_path is the full new path, e.g.
\"archive/old-guide\". Fails if the move would change the access rules of any moved
document unless allow_access_change is true: ask the user before setting it. Lists
the documents that link to the old path so you can fix them with edit_document."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Current document path, e.g. \"guides/old-name\""},
    "new_path": {"type": "string", "description": "Full new path, e.g. \"archive/new-name\"; letters, digits, '-', '_' and '/'"},
    "allow_access_change": {"type": "boolean", "description": "Confirm a move that changes which access rule applies to the moved documents", "default": false}
  },
  "required": ["path", "new_path"]
}
```

**Comportamiento**

1. Valida:
   - `path` y `new_path` no son la home;
   - `new_path` es canónica y distinta de `path`;
   - `new_path` no está dentro de `path`.
2. `authorizeDocument(path)`; si el directorio no existe → `errDocumentUnavailable`.
3. `documents.Resolve(new_path)`. Se requiere `CanAccessDocument` sobre `new_path` y
   sobre su padre, como hoy en la API. Si falta → `Access denied at new_path`, sin
   revelar si el destino existe.
4. `documents.Move(cfg, src, dst, check)`, donde `check` hace los pasos 5 y 6.
5. Para cada `Moved`: acceso en `From` y en `To`; si falla alguno → error de subárbol.
6. Para cada `Moved`, compara la regla efectiva en `From` y en `To` con
   `auth.MatchingRuleIndex` (§6.1). Si difiere en algún documento y
   `!allow_access_change` → error con hasta 3 ejemplos.
7. `documents.References(cfg, path, new_path)`, filtrado por acceso.

La regla efectiva se describe como `public`, `private` o `restricted (patrón)`. Sin
regla aplicable rige el valor por defecto del wiki: `private` si `wiki.private`, si no
`public`.

**Resultado**

```
Moved guides/old-name → archive/new-name (3 documents).
documents linking to the old path (update them with edit_document):
- guides/index
- ops/runbook
```

Sin referencias, la segunda parte es `documents linking to the old path: none`.

**Errores**

| Caso | Mensaje |
|---|---|
| Falta `new_path` | `Invalid arguments: new_path is required` |
| Origen home | `The home page cannot be moved` |
| Destino home | `Cannot move a document to the home page` |
| `new_path` no canónica | `Invalid new_path; use "archive/new-name"` (sugerencia de `SanitizePath`) |
| Misma ruta | `Invalid arguments: new_path equals path` |
| Dentro de sí mismo | `Cannot move a document inside itself` |
| Origen inexistente / sin acceso | `Document not found or access denied` |
| Sin acceso al destino | `Access denied at new_path` |
| Destino ocupado | `A document already exists at new_path` |
| Subárbol con documentos no visibles | `Cannot move: the section contains documents you cannot access` |
| Cambio de reglas | `Moving changes the access rules of 2 documents, e.g. finance/report: restricted (/finance/**) → public; ask the user and pass allow_access_change: true to confirm` |

### 5.2 `restore_version`

| | |
|---|---|
| Rol mínimo | `editor` (igual que `/api/versions`) |
| Annotations | `readOnlyHint: false`, `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false` |
| Equivale a | `POST /api/versions/{docPath}/{timestamp}/restore` |

**Descripción:** "Restores a document to a version from its history. Pass the version
timestamp and the current revision from read_document (include history lists the
versions). The current content is kept in the history, so a restore can be undone."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Document path; \"/\" for the home page"},
    "version": {"type": "string", "description": "Version timestamp (yyyymmddhhmmss) from the history", "pattern": "^[0-9]{14}$"},
    "revision": {"type": "string", "description": "Current revision returned by read_document"}
  },
  "required": ["path", "version", "revision"]
}
```

**Comportamiento**

1. Valida que `version` tenga 14 dígitos y que `revision` no esté vacía.
2. `authorizeDocument(path)`; si `document.md` no existe → `errDocumentUnavailable`.
3. Lee `VersionsDir/<version>.md`; si no existe → error.
4. `documents.Save(cfg, doc, contenidoVersión, SaveOptions{ExpectedRevision: revision})`.
   Esto guarda el contenido actual como versión nueva, aplica la regla de colisión por
   segundo de la etapa 2 y escribe de forma atómica.
5. Si el contenido actual ya es el de la versión → `Unchanged`. Así un reintento es
   idempotente aunque la revisión haya cambiado (la etapa 2 hace esta comparación
   antes que la de revisión).

A diferencia de `read_document`, no hace falta un segundo control de rol para el
historial: el rol mínimo de la tool ya es editor.

**Resultado**

```
Restored guides/intro to version 20261022101500.
revision: 7d2c0a9e4b1f3a88
previous version: 20261022211500
```

**Errores**

| Caso | Mensaje |
|---|---|
| `version` inválida | `Invalid arguments: version must be a yyyymmddhhmmss timestamp from the history` |
| Falta `revision` | `Invalid arguments: revision is required; read the document first` |
| Versión inexistente | `Version not found; include history in read_document to list them` |
| Revisión distinta | `Revision mismatch: the document changed since you read it; read it again` |
| Documento inexistente / sin acceso | `Document not found or access denied` |

### 5.3 `delete_document`

| | |
|---|---|
| Rol mínimo | `editor` |
| Annotations | `readOnlyHint: false`, `destructiveHint: true`, `idempotentHint: true`, `openWorldHint: false` |
| Equivale a | `DELETE /api/document/{docPath}` |

**Descripción:** "Permanently deletes a wiki document with its attachments, history
and comments; this cannot be undone. Pass the revision from read_document. A document
with sub-documents is only deleted with recursive: true; check them first with
list_documents. Lists the documents that link to it."

**inputSchema**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Document path, e.g. \"guides/obsolete\""},
    "revision": {"type": "string", "description": "Revision from read_document; required when the path has a document"},
    "recursive": {"type": "boolean", "description": "Also delete the sub-documents", "default": false}
  },
  "required": ["path"]
}
```

**Comportamiento**

1. `path` no es la home.
2. `authorizeDocument(path)`; si el directorio no existe → `errDocumentUnavailable`.
3. Si existe `document.md`, `revision` es obligatoria. Una carpeta sin `document.md`
   no tiene revisión y solo se borra con `recursive` si tiene hijos.
4. `documents.Delete(cfg, doc, DeleteOptions{revision, recursive}, check)`, donde
   `check` exige acceso a cada directorio del subárbol.
5. `documents.References(cfg, path, path)`, filtrado por acceso.

**Resultado**

```
Deleted guides/obsolete (2 sub-documents, 3 attachments). This cannot be undone.
documents linking to it (update them with edit_document):
- guides/index
warnings:
- the comments directory could not be removed
```

`warnings` solo aparece si `DeleteResult.Warnings` tiene elementos.

**Errores**

| Caso | Mensaje |
|---|---|
| Home | `The home page cannot be deleted` |
| Falta `revision` con `document.md` | `Invalid arguments: revision is required; read the document first` |
| Revisión distinta | `Revision mismatch: the document changed since you read it; read it again` |
| Hijos sin `recursive` | `The document has 2 sub-documents; check them with list_documents and pass recursive: true to delete them too` |
| Subárbol con documentos no visibles | `Cannot delete: the section contains documents you cannot access` |
| Inexistente / sin acceso | `Document not found or access denied` |

## 6. Cambios en el código

### 6.1 Archivos

| Archivo | Cambio |
|---|---|
| `internal/documents/documents.go` | `Move`, `Delete`, `References`, `Moved`, `DeleteOptions`, `DeleteResult`, errores; `Chtimes` de la raíz (§4.4) |
| `internal/documents/documents_test.go` | Pruebas de §9.1 |
| `internal/auth/access.go` | Exporta `MatchingRuleIndex(path string, rules []config.AccessRule) int` (`-1` = sin regla), sobre `findMatchingRule` |
| `internal/mcp/move_document.go` (nuevo) | Tool §5.1 |
| `internal/mcp/restore_version.go` (nuevo) | Tool §5.2 |
| `internal/mcp/delete_document.go` (nuevo) | Tool §5.3 |
| `internal/mcp/tools.go` | Registra las tools en el orden de §5; agrega `destructiveAnnotations` |
| `internal/mcp/server.go` | `instructions` (§6.2) |
| `internal/handlers/move.go` | `MoveDocumentHandler` calcula la ruta nueva como hoy y llama a `documents.Move` con un `check` de acceso al subárbol |
| `internal/handlers/editor.go` | La rama de directorio de `DeleteDocumentHandler` usa `documents.Delete` (`Recursive: true`, con `check` de subárbol). La rama de archivo no cambia |
| `internal/handlers/versions.go` | `handleVersionRestore` usa `documents.Save` |

La verificación de cambio de reglas (§4.1, punto 4) es solo del MCP. Que la API HTTP
también la aplique es la pregunta 3 de §12.

### 6.2 Instructions

Se agrega al texto de la etapa 2:

```
move_document and delete_document list the documents that link to the old path; fix
them with edit_document. Ask the user before passing allow_access_change or before
deleting: delete_document cannot be undone. restore_version undoes a change using the
history from read_document.
```

## 7. Interacción con las etapas anteriores

- `list_documents` y `search_documents` reflejan los cambios de inmediato: leen del
  disco sin caché.
- `read_document(..., include: ["history"])` después de mover muestra el historial en
  la ruta nueva, porque `Move` mueve `VersionsDir`.
- Si un agente tiene una `revision` de antes de un move, `edit_document` sobre la ruta
  vieja falla con `Document not found or access denied`. Es el comportamiento esperado.

## 8. Seguridad

- **Cierra el agujero del subárbol** (§1, punto 1) en el MCP y en la API HTTP: nadie
  mueve ni borra documentos que no puede ver. Los admins no se ven afectados, porque
  `CanAccessDocument` siempre les da acceso.
- **Costo del error de subárbol:** revela que hay documentos no visibles bajo una ruta
  que el usuario sí ve, pero no sus nombres ni cuántos son. Se acepta, porque sin el
  mensaje el agente no entendería el rechazo y reintentaría.
- **El cambio de reglas exige confirmación explícita** con `allow_access_change`, y la
  descripción pide al agente consultar al usuario. Un admin no la evita: el riesgo que
  cubre es la exposición accidental, no la falta de permisos.
- **Destino:** se verifica el acceso antes de mirar si existe, para no revelar
  documentos restringidos en `new_path`.
- **Referencias:** solo se listan documentos accesibles.
- **Revisión:** el borrado exige la revisión del documento raíz, así que el agente
  solo borra contenido que leyó y que nadie cambió desde entonces.
- Ninguna operación escribe fuera de `documents/`, `pages/home/`, `versions/` y
  `comments/`. Todas las rutas se resuelven con `documents.Resolve`.

## 9. Pruebas (criterios de aceptación)

### 9.1 `internal/documents`

- `Move` mueve el documento, los sub-documentos, los adjuntos, las versiones y los
  comentarios; `check` recibe cada directorio del subárbol con su `From` y `To`.
- Un `check` que devuelve error no deja nada movido ni directorios creados en el destino.
- Si falla el rename de comentarios (permisos simulados), se revierte el documento y
  las versiones.
- Mover dentro de sí mismo → `ErrInsideItself`; destino con contenido → `ErrTargetExists`;
  un rename que solo cambia mayúsculas funciona.
- `Delete` sin `Recursive` con hijos → `*ChildrenError{Count}`; con `Recursive` borra el
  documento, las versiones y los comentarios; cuenta los adjuntos.
- `Delete` con `ExpectedRevision` obsoleta → `ErrRevisionMismatch` y nada borrado.
- `References` detecta las cuatro formas de §4.5, ignora los bloques de código y los
  falsos prefijos (`/guides/older` no es referencia de `/guides/old`) y excluye el subárbol.
- Después de `Move`, de `Delete` y de un `Save` que crea, el mtime de `documents/`
  cambió y `[[nombre]]` resuelve a la ubicación nueva.

### 9.2 Tools

**`move_document`**
- Un viewer no la ve; un token `role: viewer` de un editor tampoco.
- Rename simple, move a otra sección y move a la raíz funcionan.
- Las referencias aparecen en el resultado; las de documentos restringidos para el
  usuario, no.
- `guides` con `guides/secret` restringido y un usuario sin grupo → error de subárbol;
  nada movido.
- `guides/intro` → `finance/intro` con un usuario del grupo `finance` → error de cambio
  de reglas; con `allow_access_change: true` se mueve.
- Home como origen o destino, ruta no canónica, misma ruta, dentro de sí mismo,
  destino ocupado y destino sin acceso dan los mensajes de §5.1.

**`restore_version`**
- Restaura el contenido exacto de la versión y guarda el actual como versión nueva.
- Una revisión obsoleta da mismatch, sin cambios.
- Restaurar dos veces la misma versión → la segunda devuelve `Unchanged`.
- Versión inexistente y timestamp inválido dan los mensajes de §5.2.
- Un editor sin grupo sobre `finance/report` → `errDocumentUnavailable`.

**`delete_document`**
- Borra un documento hoja con su revisión y lista las referencias.
- Sin `revision` → error; con una obsoleta → mismatch; nada borrado.
- Con hijos y sin `recursive` → error con el conteo; con `recursive: true` borra todo y
  el resultado cuenta sub-documentos y adjuntos.
- Subárbol con un documento restringido → error de subárbol; nada borrado.
- La home → error.
- Una carpeta sin `document.md` y con hijos se borra sin `revision` usando `recursive`.

### 9.3 Servidor y regresiones

- `tools/list` de un editor devuelve las 9 tools en el orden de §5; un viewer, 4.
- La API HTTP mantiene sus respuestas en los casos válidos.
- `POST /api/document/move` y `DELETE /api/document/{path}` responden `403` cuando el
  subárbol contiene documentos no visibles.
- `POST /api/document/move` devuelve error, y deja todo como estaba, si no puede mover
  el historial o los comentarios.
- Las pruebas de las etapas 1 y 2 siguen pasando.

## 10. Cambios visibles en la API HTTP

| Endpoint | Antes | Después |
|---|---|---|
| `POST /api/document/move` | Solo verificaba el acceso a la raíz | `403` si el subárbol contiene documentos no visibles |
| `POST /api/document/move` | Si fallaba mover versiones o comentarios, solo se registraba un `Warn` | Se revierte y responde `500` |
| `POST /api/document/move` | Mover dentro de sí mismo fallaba en `os.Rename` (`500`) | `400` con un mensaje claro |
| `DELETE /api/document/{path}` | Solo verificaba el acceso a la raíz | `403` si el subárbol contiene documentos no visibles |
| `POST /api/versions/…/restore` | Restaurar contenido idéntico creaba una versión duplicada; misma colisión por segundo que `/api/save` | Mismas reglas que `documents.Save` |
| Todos los anteriores | `[[nombre]]` podía resolver a una ruta vieja tras cambios anidados | El índice se invalida (§4.4) |

## 11. Documentación

- `docs/openapi.yaml`: agregar las tres tools a la sección `/mcp` y los cambios de §10
  a los endpoints HTTP.
- `rest-api-examples.http`: ejemplos de `move_document`, `restore_version` y
  `delete_document`, con un token `role: editor`.
- Actualizar la tabla de etapas en la spec de la etapa 2 (o en un índice
  `docs/specs/README.md`) para marcar el plan como completo.

## 12. Preguntas abiertas

1. **Papelera.** `delete` es irreversible y borra el historial. ¿Se mueve a
   `<root>/trash/<timestamp>/` en lugar de borrar? Haría falta una política de purga y
   una forma de restaurar para admins. Recomendación: una etapa propia, antes de dar
   tokens editor a agentes en wikis importantes.
2. **Reescritura automática de enlaces.** Un `update_links: true` en `move_document`
   ahorraría N llamadas a `edit_document`, pero editaría documentos que el agente no
   leyó, cada uno con su propia concurrencia. ¿Vale la pena?
3. **Confirmación de cambio de reglas en la API HTTP.** ¿El editor web también debe
   avisar cuando un move cambia la regla efectiva? Requiere cambiar la UI.
4. **Redirecciones.** ¿Guardar la ruta vieja para redirigir enlaces externos y
   marcadores tras un move?
5. **Enlaces relativos.** `References` no detecta `](../old)`. ¿Se resuelven los
   relativos contra la ruta de cada documento, o basta con documentar la limitación?
