# Práctica: ciclo de vida IAM en Go

Servicio didáctico para modelar un cambio de rol de Soporte a Finanzas. Usa solo la biblioteca estándar de Go y datos en memoria.

## Ejecutar

Desde PowerShell:

```powershell
cd work/iam-lifecycle-go
go run .
```

El servicio escucha en `http://localhost:8080`. La identidad inicial es Ana (`ana`), con permisos de Soporte.

## Recorrido de la práctica

En otra terminal PowerShell, primero emití una delegación limitada para un agente de IA que actúa por Ana:

```powershell
$delegation = Invoke-RestMethod -Method Post -Uri http://localhost:8080/delegations `
  -ContentType 'application/json' `
  -Body '{"identityId":"ana","agentId":"support-helper","scopes":["tickets.read"],"expiresInSeconds":300}'
$delegation.id
```

Comprobá que el acceso delegado funciona:

```powershell
Invoke-RestMethod "http://localhost:8080/delegations/$($delegation.id)/authorize?scope=tickets.read"
```

Simulá una caída del conector externo de Tickets y cambiá a Ana a Finanzas:

```powershell
Invoke-RestMethod -Method Put -Uri http://localhost:8080/demo/connectors/tickets/failure `
  -ContentType 'application/json' -Body '{"fail":true}'

$event = Invoke-RestMethod -Method Post -Uri http://localhost:8080/identities/ana/role `
  -ContentType 'application/json' `
  -Body '{"eventId":"ana-role-change-001","role":"finance"}'
$event | ConvertTo-Json -Depth 5
```

La respuesta muestra `status: partial` y `pendingSystems: ["tickets"]`: Finanzas ya recibió el nuevo acceso, Tickets todavía conserva el anterior. La delegación del agente se revoca en el Core al comenzar el cambio, por lo que esta autorización debe dar `403` aunque el conector esté caído:

```powershell
Invoke-WebRequest "http://localhost:8080/delegations/$($delegation.id)/authorize?scope=tickets.read"
```

Recuperá el conector y reintentá el evento:

```powershell
Invoke-RestMethod -Method Put -Uri http://localhost:8080/demo/connectors/tickets/failure `
  -ContentType 'application/json' -Body '{"fail":false}'

Invoke-RestMethod -Method Post -Uri http://localhost:8080/events/ana-role-change-001/retry |
  ConvertTo-Json -Depth 5
Invoke-RestMethod http://localhost:8080/identities/ana | ConvertTo-Json -Depth 5
```

El rol pasa a Finanzas cuando ambos sistemas confirmaron la aplicación. Repetir el mismo `eventId` no vuelve a ejecutar el cambio. Reutilizarlo con datos diferentes devuelve `409`.

## API resumida

| Método y ruta | Para qué sirve |
|---|---|
| `GET /healthz` | Chequeo de salud |
| `GET /identities/{id}` | Estado deseado y permisos efectivos |
| `POST /identities/{id}/role` | Solicita cambio de rol, requiere `eventId` |
| `GET /events/{id}` | Consulta resultado y conectores pendientes |
| `POST /events/{id}/retry` | Reintenta solo los conectores que faltan |
| `GET /connectors` | Estado de sistemas externos simulados |
| `PUT /demo/connectors/{name}/failure` | Activa o desactiva una falla de demo |
| `POST /delegations` | Crea delegación de permisos limitada y temporal |
| `GET /delegations/{id}/authorize?scope=...` | Evalúa acceso actual del agente |

## Decisiones de diseño

- **Mínimo privilegio:** cada rol concede una lista explícita de permisos por sistema; no se heredan permisos por defecto.
- **Idempotencia:** `eventId` evita aplicar dos veces el mismo cambio si el emisor reintenta una llamada.
- **Estado deseado y efectivo:** el Core registra el rol destino; cada conector actualiza su copia. El evento queda parcial mientras haya divergencia.
- **Reintentos seguros:** solo se reintentan los conectores que no confirmaron el evento.
- **Control de versiones:** un cambio nuevo marca como obsoletos los eventos incompletos viejos. Un retry anterior no puede restablecer accesos antiguos.
- **Delegación acotada:** el agente solo puede recibir permisos que Ana ya tiene, con vencimiento máximo de una hora. Cada autorización vuelve a comprobar revocación y acceso vigente.
- **Auditabilidad inicial:** cada evento conserva origen, destino, resultado por conector, estado y marcas de tiempo.

## Qué falta para producción

El demo omite base de datos, autenticación de las llamadas, autorización de operadores, cola durable, reconciliación periódica, límites por cliente, métricas/trazas formales y protección frente a tokens ya emitidos. La revocación inmediata aquí funciona porque cada decisión consulta el estado central en memoria; en producción hay que definir cómo se invalidan cachés, sesiones y tokens en servicios consumidores. El endpoint `/demo/.../failure` es solo un interruptor para el ejercicio y no debe exponerse en un servicio real.

Los logs actuales muestran método, ruta y duración. Una siguiente iteración puede añadir métricas como cambios completados/parciales, latencia de propagación y antigüedad del evento pendiente, además de trazas con un identificador de correlación.
