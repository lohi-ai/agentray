# Scoped lohi-ui evidence port

Source: `/Users/long/workspace/lohi/kiem-lai/web/lib/lohi-ui/src` at revision `0f9f8fa247e95aa65af722757126b2112507c41a`.

This vendored subset supports AgentRay's evidence workflow and is imported only through `@/lib/lohi-ui`. Its public primitive names and relevant APIs follow lohi-ui; presentation tokens are scoped to `.lohi-evidence` so existing AgentRay/Astryx screens keep their defaults.

AgentRay's ticket environment forbids package/lockfile changes, so this port replaces the upstream `clsx`, `tailwind-merge`, CVA, Radix Slot, and Radix Accordion internals with dependency-free equivalents. The Accordion retains native button keyboard behavior, `aria-expanded`, `aria-controls`, labelled regions, and visible focus; buttons retain 44px targets in evidence mode. Semantic status colors use 700-step text on 50-step washes, secondary text uses the design-approved AA correction, animation is disabled under reduced motion, and the host-selected dark theme is respected.
