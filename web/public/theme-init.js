(() => {
  let theme = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  try {
    const saved = localStorage.getItem('quota-watch-theme')
    if (saved === 'light' || saved === 'dark') theme = saved
  } catch {}
  document.documentElement.dataset.theme = theme
  document.documentElement.style.colorScheme = theme
  document.querySelector('meta[name="theme-color"]').content = theme === 'light' ? '#f7f9f8' : '#101010'
})()
