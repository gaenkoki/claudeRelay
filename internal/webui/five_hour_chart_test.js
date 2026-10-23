const test = require("node:test");
const assert = require("node:assert/strict");
const chart = require("./static/five-hour-chart.js");

test("groups irregular five-hour windows by account without aligning their resets", () => {
  const series = chart.buildSeries([
    { account: "beta", resets_at: 30_000, observed_cost_usd: 8 },
    { account: "alpha", resets_at: 20_000, observed_cost_usd: 4 },
    { account: "alpha", resets_at: 10_000, observed_cost_usd: 2 },
    { account: "beta", resets_at: 55_000, observed_cost_usd: 9 },
  ]);

  assert.deepEqual(series.map((value) => value.account), ["alpha", "beta"]);
  assert.deepEqual(series[0].points.map((point) => point.resetsAt), [10_000, 20_000]);
  assert.deepEqual(series[1].points.map((point) => point.resetsAt), [30_000, 55_000]);
  assert.equal(series[0].points[0].startsAt, 10_000 - chart.FIVE_HOURS_MS);
});

test("chart domain preserves actual time gaps and uses a readable value ceiling", () => {
  const series = chart.buildSeries([
    { account: "alpha", resets_at: 1_000_000, observed_cost_usd: 11 },
    { account: "alpha", resets_at: 3_000_000, observed_cost_usd: 41 },
  ]);
  const domain = chart.chartDomain(series);

  assert.ok(domain.minTime < 1_000_000);
  assert.ok(domain.maxTime > 3_000_000);
  assert.equal(domain.maxValue, 50);
});

test("invalid reset identities are omitted instead of inventing chart points", () => {
  const series = chart.buildSeries([
    { account: "alpha", resets_at: 0, observed_cost_usd: 7 },
    { account: "alpha", resets_at: "not-a-time", observed_cost_usd: 8 },
  ]);

  assert.deepEqual(series, []);
  assert.equal(chart.chartDomain(series), null);
});

test("all accounts remain available and searchable without a fixed limit", () => {
  const windows = Array.from({ length: 1_000 }, (_, index) => ({
    account: `account-${String(index).padStart(4, "0")}`,
    resets_at: 1_000_000 + index * 10_000,
    observed_cost_usd: index / 10,
  }));
  const series = chart.buildSeries(windows);

  const unfiltered = chart.accountChoices(series, "");
  assert.equal(unfiltered.matchCount, 1_000);
  assert.equal(unfiltered.choices.length, 1_000);
  const searched = chart.accountChoices(series, "account-0001");
  assert.equal(searched.matchCount, 1);
  assert.equal(searched.choices[0].account, "account-0001");
});
