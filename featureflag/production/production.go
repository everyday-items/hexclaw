// Package production links every production feature-flag owner into release tooling.
// Keep this list explicit so release auditing cannot depend on incidental imports.
package production

import (
	_ "github.com/hexagon-codes/hexclaw/adapter"
	_ "github.com/hexagon-codes/hexclaw/agents"
	_ "github.com/hexagon-codes/hexclaw/config"
	_ "github.com/hexagon-codes/hexclaw/engine"
	_ "github.com/hexagon-codes/hexclaw/eval"
	_ "github.com/hexagon-codes/hexclaw/knowledge"
	_ "github.com/hexagon-codes/hexclaw/mcp"
	_ "github.com/hexagon-codes/hexclaw/plugin"
	_ "github.com/hexagon-codes/hexclaw/skill"
)
