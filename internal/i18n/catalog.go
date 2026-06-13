package i18n

// catalog holds every UI string. Keys are grouped by surface. The catalog test
// enforces that every entry has both languages.
var catalog = map[string]entry{
	// CLI frame
	"cli.tagline":     {en: "vichu — observable agentic workflow orchestration", es: "vichu — orquestación observable de workflows agentic"},
	"cli.usage":       {en: "Usage: vichu <command> [flags]", es: "Uso: vichu <comando> [flags]"},
	"cli.commands":    {en: "Commands:", es: "Comandos:"},
	"cli.help_hint":   {en: "Run 'vichu <command> -h' for command-specific flags.", es: "Ejecuta 'vichu <comando> -h' para ver los flags de cada comando."},
	"cli.unknown_cmd": {en: "unknown command %q", es: "comando desconocido %q"},
	"cli.error":       {en: "error: ", es: "error: "},

	// Command summaries
	"cmd.init":     {en: "Detect the stack and create vichu.yaml (requires git)", es: "Detecta el stack y crea vichu.yaml (requiere git)"},
	"cmd.doctor":   {en: "Validate dependencies, adapters, and configuration", es: "Valida dependencias, adapters y configuración"},
	"cmd.run":      {en: "Run a workflow over the current repository", es: "Ejecuta un workflow sobre el repositorio actual"},
	"cmd.status":   {en: "Show a run's status (use --watch to follow)", es: "Muestra el estado de un run (--watch para seguirlo)"},
	"cmd.resume":   {en: "Resume a blocked or paused run", es: "Reanuda un run bloqueado o pausado"},
	"cmd.cancel":   {en: "Cancel a run", es: "Cancela un run"},
	"cmd.adapters": {en: "List agent adapters and their availability", es: "Lista los adapters de agentes y su disponibilidad"},
	"cmd.config":   {en: "Show the resolved configuration", es: "Muestra la configuración del proyecto"},
	"cmd.version":  {en: "Print version information", es: "Muestra la versión"},

	// init
	"init.done":          {en: "Initialized VichuFlow in %s", es: "VichuFlow inicializado en %s"},
	"init.language":      {en: "language", es: "lenguaje"},
	"init.test":          {en: "test", es: "test"},
	"init.lint":          {en: "lint", es: "lint"},
	"init.wrote":         {en: "wrote", es: "creó"},
	"init.updated_gi":    {en: "updated    .gitignore (added %s/)", es: "actualizó  .gitignore (agregó %s/)"},
	"init.next":          {en: "Next: review vichu.yaml, then run `vichu run \"<your task>\"`.", es: "Siguiente: revisa vichu.yaml y ejecuta `vichu run \"<tu tarea>\"`."},
	"init.exists":        {en: "%s already exists (use --force to overwrite)", es: "%s ya existe (usa --force para sobreescribir)"},
	"init.no_git":        {en: "git is not installed or not on PATH; install git first", es: "git no está instalado o no está en el PATH; instala git primero"},
	"init.flag_force":    {en: "overwrite an existing vichu.yaml", es: "sobreescribe un vichu.yaml existente"},
	"common.unknown_val": {en: "unknown", es: "desconocido"},

	// run
	"run.running":       {en: "Running workflow %q on: %s", es: "Ejecutando workflow %q para: %s"},
	"run.need_task":     {en: "provide a task, e.g. vichu run \"add password reset\"", es: "indica una tarea, p. ej. vichu run \"agregar reset de contraseña\""},
	"run.no_config":     {en: "no vichu.yaml found — run `vichu init` first", es: "no se encontró vichu.yaml — ejecuta `vichu init` primero"},
	"run.observe":       {en: "Observe: vichu status %s", es: "Observa: vichu status %s"},
	"run.flag_workflow": {en: "workflow to run (default: from vichu.yaml)", es: "workflow a ejecutar (default: el de vichu.yaml)"},
	"run.flag_provider": {en: "workflow provider label", es: "etiqueta de provider del workflow"},

	// status
	"status.run":           {en: "Run %s", es: "Run %s"},
	"status.status":        {en: "status", es: "estado"},
	"status.workflow":      {en: "workflow", es: "workflow"},
	"status.stage":         {en: "stage", es: "etapa"},
	"status.worker":        {en: "worker", es: "worker"},
	"status.next":          {en: "next", es: "siguiente"},
	"status.blocked":       {en: "blocked", es: "bloqueado"},
	"status.budget":        {en: "budget", es: "presupuesto"},
	"status.budget_line":   {en: "%d agent call(s), $%.2f, %.0fs, %d tokens", es: "%d llamada(s) a agentes, $%.2f, %.0fs, %d tokens"},
	"status.lock_orphaned": {en: "lock:    orphaned (owner pid %d gone) — safe to `vichu resume %s`", es: "lock:    huérfano (el pid %d ya no existe) — es seguro `vichu resume %s`"},
	"status.lock_held":     {en: "lock:    held by pid %d (run active)", es: "lock:    en uso por el pid %d (run activo)"},
	"status.recent":        {en: "recent events:", es: "eventos recientes:"},
	"status.gate_excerpt":  {en: "gate output (tail): %s", es: "salida del gate (final): %s"},
	"status.flag_watch":    {en: "follow the run until it settles (terminal, blocked, or paused)", es: "sigue el run hasta que se estabilice (terminal, bloqueado o pausado)"},
	"status.flag_interval": {en: "refresh interval for --watch", es: "intervalo de refresco para --watch"},
	"status.no_runs":       {en: "no runs yet — start one with `vichu run`", es: "aún no hay runs — inicia uno con `vichu run`"},
	"status.not_found":     {en: "run %q not found", es: "no se encontró el run %q"},

	// resume / cancel
	"resume.resuming":    {en: "Resuming run %s", es: "Reanudando run %s"},
	"resume.flag_accept": {en: "accept external changes: re-baseline the workspace snapshot and continue", es: "acepta cambios externos: rebasea el snapshot del workspace y continúa"},
	"cancel.done":        {en: "Canceled run %s.", es: "Run %s cancelado."},
	"cancel.already":     {en: "Run %s is already %s.", es: "El run %s ya está %s."},

	// adapters / doctor
	"adapters.header":    {en: "Agent adapters:", es: "Adapters de agentes:"},
	"adapters.available": {en: "available", es: "disponible"},
	"doctor.header":      {en: "VichuFlow doctor (%s/%s, go %s)", es: "VichuFlow doctor (%s/%s, go %s)"},
	"doctor.adapters":    {en: "adapters:", es: "adapters:"},
	"doctor.all_ok":      {en: "All required checks passed.", es: "Todas las verificaciones requeridas pasaron."},
	"doctor.failures":    {en: "Some checks failed — see above.", es: "Algunas verificaciones fallaron — revisa arriba."},
	"doctor.git_ok":      {en: "available", es: "disponible"},
	"doctor.git_missing": {en: "not installed (required)", es: "no instalado (requerido)"},
	"doctor.no_config":   {en: "not found — run `vichu init`", es: "no encontrado — ejecuta `vichu init`"},

	// config
	"config.header": {en: "Config: %s", es: "Configuración: %s"},

	// engine progress
	"engine.stage":         {en: "stage: %s", es: "etapa: %s"},
	"engine.completed":     {en: "run completed", es: "run completado"},
	"engine.canceled":      {en: "run canceled", es: "run cancelado"},
	"engine.blocked":       {en: "run blocked: %s", es: "run bloqueado: %s"},
	"engine.failed":        {en: "run failed: %s", es: "run fallido: %s"},
	"engine.dirty_warning": {en: "warning: starting with %d uncommitted change(s)", es: "advertencia: iniciando con %d cambio(s) sin commitear"},
	"engine.no_gates":      {en: "verify: no gates configured (set commands.test/lint/typecheck in vichu.yaml)", es: "verify: no hay gates configurados (define commands.test/lint/typecheck en vichu.yaml)"},
	"engine.rebaselined":   {en: "workspace re-baselined: external changes accepted", es: "workspace rebaseado: cambios externos aceptados"},
	"engine.drift_hint":    {en: "workspace drift detected — re-run with `vichu resume --accept-changes %s` to accept external changes", es: "se detectó drift del workspace — usa `vichu resume --accept-changes %s` para aceptar los cambios externos"},
	"version.line":         {en: "vichu %s", es: "vichu %s"},
	"version.commit":       {en: "  commit: %s", es: "  commit: %s"},
	"version.built":        {en: "  built:  %s", es: "  compilado: %s"},
}
