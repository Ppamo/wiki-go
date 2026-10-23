# Specs

## Servidor MCP

El plan de tres etapas del servidor MCP está completo.

| Etapa | Spec | Tools | Estado |
|---|---|---|---|
| 1. Lectura | Sin spec (commit `20c3986`) | `search_documents`, `list_documents`, `read_document` | Implementada |
| 2. Edición | [mcp-etapa-2-edicion.md](mcp-etapa-2-edicion.md) | `edit_document`, `write_document`, `add_comment` | Implementada |
| 3. Estructura e historial | [mcp-etapa-3-estructura.md](mcp-etapa-3-estructura.md) | `move_document`, `restore_version`, `delete_document` | Implementada |

Lo que queda pendiente está en las preguntas abiertas de cada spec: §11 de la etapa 2
y §12 de la etapa 3 (papelera, reescritura de enlaces, confirmación de cambio de
reglas en la API HTTP, redirecciones y enlaces relativos).
