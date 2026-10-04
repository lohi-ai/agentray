# Scoped lohi-ui evidence port

Source: `/Users/long/workspace/lohi/kiem-lai/web/lib/lohi-ui/src` at revision `0f9f8fa247e95aa65af722757126b2112507c41a`.

This vendored subset supports AgentRay's evidence workflow and is imported only through `@/lib/lohi-ui`. Its public primitive names and relevant APIs follow lohi-ui; presentation tokens are scoped to `.lohi-evidence` so existing AgentRay/Astryx screens keep their defaults.

This repair keeps `package.json` and the lockfile frozen as required by the ticket. The exact upstream Accordion component and `cn` behavior are retained; their MIT-licensed Radix Accordion, `clsx`, and `tailwind-merge` runtime sources are vendored under `src/vendor/` so CI has no sibling-checkout or undeclared-package dependency. The Radix bundle is generated from `@radix-ui/react-accordion` 1.2.13 with React externalized, and the component source differs from revision `0f9f8fa` only in that its Radix import points at that local bundle.

The Accordion therefore retains Radix single/multiple, collapsible, controlled/uncontrolled, keyboard navigation, disabled, orientation, and direction APIs. Buttons retain 44px targets in evidence mode. Semantic status colors use 700-step text on 50-step washes, secondary text uses the design-approved AA correction, animation is disabled under reduced motion, and the host-selected dark theme is respected.
