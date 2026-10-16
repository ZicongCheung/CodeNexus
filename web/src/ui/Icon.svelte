<script>
  // Inline SVG instead of an icon font: no extra request, no flash of missing
  // glyphs, and stroke weights can follow the design tokens.
  let { name, size = 18, stroke = 1.75, class: cls = '' } = $props()

  const paths = {
    accounts: ['M16 19v-1.5a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4V19', 'M9 10.5a3.25 3.25 0 1 0 0-6.5 3.25 3.25 0 0 0 0 6.5Z', 'M17 4.3a3.25 3.25 0 0 1 0 6.2', 'M18.5 19v-1.5a4 4 0 0 0-2.6-3.75'],
    tasks: ['M9 5.5h11', 'M9 12h11', 'M9 18.5h11', 'M3.5 5.5l1.2 1.2L7 4.6', 'M3.5 12l1.2 1.2L7 11.1', 'M3.5 18.5l1.2 1.2L7 17.6'],
    logs: ['M3 12h3.5l2.5-6 4 12 2.5-6H21'],
    usage: ['M4 20V10', 'M10 20V4', 'M16 20v-7', 'M22 20H2'],
    add: ['M12 5v14', 'M5 12h14'],
    importAcc: ['M12 3v11', 'm7.5 9.5 4.5 4.5 4.5-4.5', 'M4 20h16'],
    settings: ['M4 7h10', 'M18 7h2', 'M4 17h6', 'M14 17h6', 'M16 7a2 2 0 1 0-4 0 2 2 0 0 0 4 0Z', 'M12 17a2 2 0 1 0-4 0 2 2 0 0 0 4 0Z'],
    sun: ['M12 4V2', 'M12 22v-2', 'M4 12H2', 'M22 12h-2', 'M5.6 5.6 4.2 4.2', 'M19.8 19.8l-1.4-1.4', 'M18.4 5.6l1.4-1.4', 'M4.2 19.8l1.4-1.4', 'M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10Z'],
    moon: ['M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z'],
    monitor: ['M3 5h18v11H3z', 'M8 20h8', 'M12 16v4'],
    close: ['M6 6l12 12', 'M18 6 6 18'],
    chevron: ['m6 9 6 6 6-6'],
    check: ['m5 12.5 4.5 4.5L19 7'],
    alert: ['M12 8v5', 'M12 16.5h.01', 'M12 3.5 1.8 20.5h20.4L12 3.5Z'],
    info: ['M12 16v-5', 'M12 8h.01', 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z'],
    refresh: ['M20 11a8 8 0 1 0-1.6 5.6', 'M20 4v7h-7'],
    external: ['M14 4h6v6', 'M20 4l-8 8', 'M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4'],
    eye: ['M2 12s3.6-6.5 10-6.5S22 12 22 12s-3.6 6.5-10 6.5S2 12 2 12Z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z'],
    eyeOff: ['M4 4l16 16', 'M9.9 5.8A9.6 9.6 0 0 1 12 5.5c6.4 0 10 6.5 10 6.5a17 17 0 0 1-3.4 4.2', 'M6.3 7.9A17 17 0 0 0 2 12s3.6 6.5 10 6.5c1.3 0 2.5-.3 3.5-.7', 'M10 10a2.8 2.8 0 0 0 4 4'],
    trash: ['M4 7h16', 'M9 7V5h6v2', 'M6 7l1 13h10l1-13', 'M10 11v6', 'M14 11v6'],
    search: ['M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16Z', 'm21 21-4.3-4.3'],
    key: ['M15.5 8.5a3.5 3.5 0 1 0 0 .01', 'm13 11-9 9v3h3l1-1v-2h2v-2h2l1-1'],
    bolt: ['M13 2 4 14h6l-1 8 9-12h-6l1-8Z'],
  }
</script>

<svg
  class={cls}
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width={stroke}
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"
>
  {#each paths[name] ?? [] as d}
    <path {d} />
  {/each}
</svg>
