import test from 'node:test';
import assert from 'node:assert/strict';
import { forecastDatabaseGrowth } from '../src/capacity.js';

test('capacity forecast requires three days and uses conservative daily growth', () => {
  assert.equal(forecastDatabaseGrowth([{ day: '2026-11-15T00:00:00Z', databaseBytes: 100 }]).status, 'insufficient');
  const forecast = forecastDatabaseGrowth([
    { day: '2026-11-15T00:00:00Z', databaseBytes: 100, sampleId: 'first' },
    { day: '2026-11-16T00:00:00Z', databaseBytes: 120, sampleId: 'middle' },
    { day: '2026-11-17T00:00:00Z', databaseBytes: 150, sampleId: 'latest' }
  ]);
  assert.equal(forecast.status, 'estimate');
  assert.equal(forecast.dailyGrowthBytes, 25);
  assert.equal(forecast.projected30DayBytes, 900);
  assert.equal(forecast.confidence, 'low');
  assert.equal(forecast.latestSampleId, 'latest');
  const shrinking = forecastDatabaseGrowth([
    { day: '2026-11-15T00:00:00Z', databaseBytes: 150 },
    { day: '2026-11-16T00:00:00Z', databaseBytes: 120 },
    { day: '2026-11-17T00:00:00Z', databaseBytes: 100 }
  ]);
  assert.equal(shrinking.dailyGrowthBytes, 0);
});
