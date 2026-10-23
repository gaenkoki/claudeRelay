(function attachFiveHourChart(root, factory) {
  const chart = factory();
  if (typeof module === "object" && module.exports) module.exports = chart;
  else root.ClaudeRelayFiveHourChart = chart;
})(typeof globalThis === "object" ? globalThis : this, function createFiveHourChart() {
  const FIVE_HOURS_MS = 5 * 60 * 60 * 1000;

  function finiteNumber(value) {
    const number = Number(value);
    return Number.isFinite(number) ? number : null;
  }

  function buildSeries(windows) {
    const grouped = new Map();
    for (const window of Array.isArray(windows) ? windows : []) {
      const resetsAt = finiteNumber(window?.resets_at);
      if (resetsAt === null || resetsAt <= 0) continue;
      const rawValue = finiteNumber(window?.observed_cost_usd);
      const account = String(window?.account || "—");
      const point = {
        account,
        resetsAt,
        startsAt: resetsAt - FIVE_HOURS_MS,
        value: Math.max(0, rawValue === null ? 0 : rawValue),
        window,
      };
      if (!grouped.has(account)) grouped.set(account, []);
      grouped.get(account).push(point);
    }
    return [...grouped.entries()]
      .sort(([left], [right]) => left.localeCompare(right, "zh-CN"))
      .map(([account, points]) => ({
        account,
        points: points.sort((left, right) => left.resetsAt - right.resetsAt),
      }));
  }

  function niceUpperBound(value) {
    const safe = finiteNumber(value);
    if (safe === null || safe <= 0) return 1;
    const exponent = 10 ** Math.floor(Math.log10(safe));
    const normalized = safe / exponent;
    const step = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;
    return step * exponent;
  }

  function chartDomain(series) {
    const points = (Array.isArray(series) ? series : []).flatMap((value) => value.points || []);
    if (points.length === 0) return null;
    let minTime = Math.min(...points.map((point) => point.resetsAt));
    let maxTime = Math.max(...points.map((point) => point.resetsAt));
    if (minTime === maxTime) {
      minTime -= FIVE_HOURS_MS / 2;
      maxTime += FIVE_HOURS_MS / 2;
    } else {
      const padding = Math.max((maxTime - minTime) * 0.035, 15 * 60 * 1000);
      minTime -= padding;
      maxTime += padding;
    }
    const maxValue = Math.max(...points.map((point) => point.value), 0);
    return {
      minTime,
      maxTime,
      maxValue: niceUpperBound(maxValue * 1.08),
    };
  }

  function seriesByRecency(series) {
    return [...(Array.isArray(series) ? series : [])].sort((left, right) => {
      const leftLatest = Math.max(...(left.points || []).map((point) => point.resetsAt), 0);
      const rightLatest = Math.max(...(right.points || []).map((point) => point.resetsAt), 0);
      return rightLatest - leftLatest || left.account.localeCompare(right.account, "zh-CN");
    });
  }

  function accountChoices(series, query) {
    const normalized = String(query || "").trim().toLocaleLowerCase("zh-CN");
    const matches = seriesByRecency(series).filter((value) =>
      !normalized || value.account.toLocaleLowerCase("zh-CN").includes(normalized));
    return { choices: matches, matchCount: matches.length };
  }

  return { FIVE_HOURS_MS, buildSeries, chartDomain, niceUpperBound, accountChoices };
});
