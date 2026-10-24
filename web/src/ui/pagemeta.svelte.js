// The one-line summary the top bar prints after the page name. Each view
// knows its own answer — the overview counts accounts, settings counts
// sections — and the shell only knows which view is mounted. A tiny shared
// cell is cheaper than teaching the shell to fetch the config schema twice.

export const pageMeta = $state({ text: '' })

export function setPageMeta(text) {
  pageMeta.text = text || ''
}
