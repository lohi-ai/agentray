# Airplane Collision Simulator (web page)

Build a **single, self-contained HTML file** that visually simulates **two
airplanes flying toward each other, colliding, and exploding**.

## Requirements

1. **Single file**: one HTML document with all CSS and JavaScript inline. No
   external assets, fonts, libraries, or network requests of any kind.
2. **Canvas animation**: a `<canvas>` scene animated with
   `requestAnimationFrame`:
   - a sky background (gradient, clouds — your call),
   - two visibly distinct airplanes entering from opposite sides on courses
     that intersect,
   - smooth motion toward the collision point,
   - a **collision detection** check (distance or bounding-box based — an
     actual computed check, not a hard-coded timer),
   - an explosion effect on impact (expanding particles / fireball / smoke),
   - debris or the planes falling after the impact.
3. **Controls**: a Restart button that resets the simulation, and a
   speed control (slider or buttons) that changes how fast the planes fly.
4. **HUD**: show each plane's current speed and the distance between the
   planes, updating live; show a clear "COLLISION" indicator at impact.
5. **Robustness**: no JavaScript errors; the page must work when opened
   directly from disk (file://) in a modern browser.

## Working style (mandatory)

Deliver the page **incrementally, one milestone per step**, calling
`write_file` with the **complete current page** (path `collision.html`) after
each milestone:

1. Milestone 1 — static scene: canvas, sky, two parked airplane sprites.
2. Milestone 2 — flight: both planes animate toward each other; HUD shows
   speed and distance.
3. Milestone 3 — impact: collision detection triggers the explosion and
   debris.
4. Milestone 4 — polish: restart + speed controls, visual polish, final
   cleanup.

After milestone 4, reply with exactly: `SHIPPED`.
