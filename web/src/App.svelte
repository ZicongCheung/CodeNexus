<script>
  // Placeholder shell. The unified console (sources as a first-class concept)
  // is built out in the panel phase.
  let service = $state('codenexus')
  let status = $state('loading')

  async function ping() {
    try {
      const res = await fetch('../healthz')
      const body = await res.json()
      service = body.service ?? service
      status = body.status ?? 'unknown'
    } catch (err) {
      status = 'unreachable'
    }
  }

  ping()
</script>

<main>
  <h1>CodeNexus</h1>
  <p>控制台脚手架（Svelte 5 + Vite）。</p>
  <dl>
    <dt>service</dt><dd>{service}</dd>
    <dt>status</dt><dd>{status}</dd>
  </dl>
</main>

<style>
  main {
    font-family: ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
    max-width: 40rem;
    margin: 4rem auto;
    padding: 0 1.5rem;
  }
  h1 { font-size: 1.4rem; }
  p { color: #6b7280; }
  dl { display: grid; grid-template-columns: 6rem 1fr; gap: .35rem 1rem; margin-top: 1.5rem; }
  dt { color: #6b7280; }
  dd { margin: 0; font-variant-numeric: tabular-nums; }
</style>
