import test from 'node:test';
import assert from 'node:assert/strict';
import { inspectionReportHtml, inspectionReportMarkdown } from '../src/reports.js';

test('inspection reports cite sample and event IDs and escape untrusted HTML', () => {
  const run = { id: 'run-1', assetId: 'asset-1', templateId: 'pg_daily', templateVersion: 2,
    sampleId: 'sample-1', eventIds: ['event-1'], startedAt: '2026-11-17T00:00:00Z',
    completedAt: '2026-11-17T00:00:01Z', status: 'completed',
    summary: { risk: 1, pass: 0, unknown: 0, changed: 1 },
    findings: [{ title: 'bad | check', status: 'risk', observed: 1, threshold: 1,
      source: '<script>alert(1)</script>', previousStatus: 'pass' }] };
  const asset = { id: 'asset-1', name: '<script>alert(1)</script>' };
  const markdown = inspectionReportMarkdown(run, asset);
  assert.match(markdown, /sample-1/);
  assert.match(markdown, /event-1/);
  assert.match(markdown, /bad \\\| check/);
  const html = inspectionReportHtml(run, asset);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});
