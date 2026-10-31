package artifactsmcp

// Guides are task-specific playbooks returned by artifact_docs({guide}).
// Keep the list in Docs ("## Guides") in step with this map.
var Guides = map[string]string{
	"paper-explainer": PaperExplainerGuide,
}

// PaperExplainerGuide teaches the pinned-figure "scrollytelling" pattern for
// converting or summarizing a white paper, spec, design doc, or plan.
const PaperExplainerGuide = `# Guide: paper explainer (scrollytelling)

Use this when asked to convert, summarize, or explain a white paper, research
paper, spec, RFC, design doc, or plan as an artifact. The pattern: ONE central
figure stays pinned at the top of the view while the text scrolls underneath
it, and the figure changes with the text: elements are highlighted,
annotated, added, or animated so the reader always sees exactly what the
paragraph in front of them is talking about.

## 1. Read and plan before writing HTML

1. Read the whole source. Extract: the problem, the core idea, the mechanism
   (how it works, step by step), the key results with their actual numbers,
   limitations, and why it matters.
2. Pick the ONE figure that carries the paper. Usually it is the mechanism:
   - a protocol, request flow, or plan -> a sequence diagram (actors as
     columns, messages as arrows in time order);
   - an architecture -> a box-and-arrow diagram;
   - a pipeline/algorithm -> a flow of stages, or the data structure being
     transformed (a tape, a grid, a tree);
   - an empirical paper -> its headline plot, rebuilt as SVG.
   You may swap the figure at a section boundary (e.g. mechanism diagram,
   then a results chart), but keep each figure on screen for several steps;
   a new figure every paragraph defeats the pattern.
3. Write the step list (typically 6-15 steps). Each step is one short
   paragraph (2-5 sentences) plus exactly one figure state. For every step
   write down what changes in the figure: which elements appear, which are
   highlighted, which get a callout, what moves. If a step changes nothing in
   the figure, merge it into a neighbour or move it out of the scrolly part.
4. Accuracy rules: use the source's real numbers, names, and terminology;
   never invent results. If you simplify or interpret, say so ("roughly",
   "in our reading"). Cite the paper (title, authors, year, link) in the
   header and put a short "What we left out" note at the end.

## 2. Page structure

    header      title, authors/source + link, a 2-3 sentence TL;DR
    .scrolly    the pinned figure (.stage) followed by the steps (.step)
    after       results table / limitations / what we left out / glossary

- The stage is position: sticky; top: 0 and takes roughly 45-55% of the
  viewport height (height: min(55vh, 520px)), full width, with a solid
  background and a subtle bottom border/shadow so text scrolls cleanly
  beneath it. The figure is an inline <svg> with a viewBox that scales to
  fit (width: 100%; height: 100%; preserveAspectRatio="xMidYMid meet").
- Steps sit below the stage in a readable column (max-width ~ 680px).
  Give each step enough bottom margin (about 40-60vh) that only one is
  "current" at a time; the last step needs extra space so it can become
  active.
- The chat preview shows the top 1280x800 of the page: keep the header
  short so the figure, in its meaningful step-0 overview state, is visible
  in the preview.
- Put a step counter or a thin progress bar in the stage ("3 / 9 - Copy
  loop"), and a one-line caption that restates the current step's point.
- Must still work on a phone (~360px wide): the stage stays on top; shrink
  labels, never side-by-side columns below ~900px.

## 3. Driving the figure from scroll

Give every figure element that changes an id or class, and drive all state
from ONE function, setStep(n), that is a pure function of n: jumping
straight to step 7 must produce the same picture as scrolling through 1-6.
Declarative attributes keep this simple:

    data-show="3-"     visible from step 3 on (also "2-5", "4")
    data-hl="5,6"      highlighted at steps 5 and 6
    data-note="4"      annotation/callout shown only at step 4

Minimal runtime (inline it; no libraries needed):

    <script>
    const steps = [...document.querySelectorAll('.step')];
    const inRange = (spec, n) => spec.split(',').some(part => {
      const [a, b] = part.split('-');
      const lo = +a, hi = b === undefined ? lo : (b === '' ? Infinity : +b);
      return n >= lo && n <= hi;
    });
    function setStep(n) {
      document.querySelectorAll('[data-show]').forEach(el => el.classList.toggle('on', inRange(el.dataset.show, n)));
      document.querySelectorAll('[data-hl]').forEach(el => el.classList.toggle('hl', inRange(el.dataset.hl, n)));
      document.querySelectorAll('[data-note]').forEach(el => el.classList.toggle('on', inRange(el.dataset.note, n)));
      steps.forEach((s, i) => s.classList.toggle('active', i === n));
      document.querySelector('.stage').dataset.step = n;
      // step-specific animation hooks: (onStep[n] || (() => {}))();
    }
    // A step becomes current as it crosses the band just below the stage.
    const io = new IntersectionObserver(entries => {
      for (const e of entries) if (e.isIntersecting) setStep(steps.indexOf(e.target));
    }, { rootMargin: '-60% 0px -35% 0px' });
    steps.forEach(s => io.observe(s));
    setStep(0);
    </script>

CSS for the states:

    [data-show], [data-note] { opacity: 0; transition: opacity .4s, transform .4s; }
    [data-show].on, [data-note].on { opacity: 1; }
    .hl { stroke: var(--accent); stroke-width: 3; filter: drop-shadow(0 0 4px var(--accent)); }
    .step { opacity: .35; transition: opacity .3s; }
    .step.active { opacity: 1; }
    @media (prefers-reduced-motion: reduce) { * { transition: none !important; animation: none !important; } }

Animation: prefer CSS transitions on opacity/transform/stroke-dashoffset
(drawing an arrow: set stroke-dasharray to its length and transition
stroke-dashoffset to 0). For a looping process (data moving along an arrow,
a head walking a tape), run a requestAnimationFrame or setInterval loop that
starts in setStep for that step and is cancelled when the step changes.
Animations must be idempotent and restartable when the reader scrolls back.

Interaction (optional, cheap, worthwhile): clicking a figure element whose
data-step attribute names its explaining step scrolls that step into view
(steps[k].scrollIntoView({behavior: 'smooth', block: 'center'})); arrow
keys / j k move between steps.

## 4. Sequence diagrams (plans, protocols, request flows)

- Actors are columns with a header box and a dashed lifeline; time flows
  downward. Number every message ("1", "2", ...) and keep the numbers
  identical to the step text, so "in step 4 the daemon retries" points at
  arrow 4.
- Reveal messages in order: message k gets data-show="k-", so the diagram
  grows as the reader scrolls; the current message gets data-hl="k" and a
  callout beside the arrow saying what is carried (payload, ids, timeouts).
- Group phases (setup, happy path, failure/retry, teardown) with a faint
  labelled band behind the messages; highlight the band when the text
  introduces the phase.
- Show failure paths as dashed red arrows revealed in their own step, and
  alternatives (alt/opt) as labelled boxes, rather than cramming every branch
  into the first view.
- If the diagram outgrows the stage height, scroll or translate the SVG
  viewport (animate the viewBox y offset) so the current message stays in
  view, with earlier ones fading rather than disappearing.
- Activation bars (thin rectangles on a lifeline) show who is busy; show
  them only for the steps where that matters.

Worked pattern (from a NAC captive-portal methods plan): the sequence
diagram is the SCROLLING part and the pinned figure is the network
topology. Each chapter is one data object; every message row is a scroll
stop that tells the topology what to light up:

    const CH = [{
      id: "m2", title: "ACL with DNAT actions",
      nodes: { C: {t: "client", x: 40, y: 205, l: "Client"},
               CT: {t: "switch", x: 232, y: 205, l: "Controller"},
               NAC: {t: "nac", x: 232, y: 92, l: "NAC"}, ... },
      links: [["C", "AP", "dash"], ["AP", "CT"], ["CT", "NAC"], ...],
      base: "Caption shown before any step is active.",
      lanes: [["C", "Client"], ["A", "Controller"], ["N", "NAC proxy"]],
      rows: [
        {note: ["A", "N"], text: "Role ACL: ... dst-nat ..."},       // not a stop
        {m: ["C", "A"], k: "http", l: "GET http://captive.apple.com/",
         topo: {p: ["C", "AP", "CT"], tags: [["CT", "matches svc-http"]],
                cap: "Probe arrives at the controller."}},
        {m: ["A", "N"], k: "nat", l: "dst-nat: destination -> NAC",
         topo: {p: ["CT", "NAC"], tags: [["CT", "dst-nat"]], cap: "..."}},
        {frame: "alt: any other HTTPS"}, {guard: "[SNI on allow-list]"},
        {m: ["N", "C"], k: "bad", l: "drop / wrong cert",
         topo: {p: ["NAC", "CT", "AP", "C"], tags: [["C", "cert error"]]}},
      ],
    }];

- Generate BOTH the lane diagram rows and the topology SVG from this data;
  never hand-draw them separately, so they cannot drift apart.
- topo.p is the path of the current message through the topology (drawn
  as one arrowed path, styled by the message kind k: ok / redirect / nat /
  bad), topo.tags are callouts next to nodes, topo.hot highlights nodes
  with no path, and topo.cap replaces the pinned caption.
- Rows already scrolled past stay visible ("past"); rows below the trigger
  line are dimmed ("future"), which reveals the sequence one rung at a time.
- Instead of an IntersectionObserver you can run a requestAnimationFrame-
  throttled scroll handler that marks the last row whose top is above a
  trigger line just under the sticky figure
  (stickyHeight + 0.42 * (innerHeight - stickyHeight)) as active. This copes
  well with many short rows and with multiple chapters, each with its own
  sticky figure.

## 5. Visual style

- Self-contained: inline CSS/JS/SVG, no external assets (see artifact_docs).
- Set your own colours. One accent colour for "current", one muted colour
  for "already seen", a neutral for "not yet relevant"; red only for errors.
- Text in the SVG must stay legible at phone width: >= 12px rendered; put
  long explanations in the step text, not the figure.
- Use the paper's own symbols and names in the figure so readers can map
  back to the source.

## 6. Checklist before publishing

- Every step changes the figure, and every figure change is explained by its
  step.
- Scrolling up and down, and reloading mid-page, always shows the right
  state (setStep is pure).
- Numbers and claims match the source; interpretation is labelled.
- Works at 360px and at 1600px wide, and with prefers-reduced-motion.
- Header + step-0 figure look good in the top 1280x800 (the chat preview).
- Publish with a specific title, e.g. "Computational Life - explainer".
`
