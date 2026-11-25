(() => {
  const destination = document.getElementById('dbsphere-grafana-return');
  if (window.top === window && destination?.origin === window.location.origin) {
    window.location.replace(destination.href);
  }
})();
