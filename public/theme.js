(() => {
  const key = 'dbsphere.theme';
  const root = document.documentElement;
  let theme = 'light';
  try {
    if (localStorage.getItem(key) === 'dark') theme = 'dark';
  } catch {
    // The theme remains available for this page when storage is disabled.
  }
  root.dataset.theme = theme;
  document.addEventListener('DOMContentLoaded', () => {
    const select = document.getElementById('theme-select');
    select.value = theme;
    select.addEventListener('change', () => {
      theme = select.value === 'dark' ? 'dark' : 'light';
      root.dataset.theme = theme;
      try { localStorage.setItem(key, theme); } catch {}
    });
    window.addEventListener('storage', event => {
      if (event.key !== key && event.key !== null) return;
      theme = event.newValue === 'dark' ? 'dark' : 'light';
      root.dataset.theme = select.value = theme;
    });
  });
})();
