import { createHash, randomUUID } from 'node:crypto';

const numeric = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;

export function sanitizePlan(input, queryId, observedAt = new Date().toISOString()) {
  if (!/^-?\d{1,20}$/.test(String(queryId)) || String(queryId) === '0' ||
    !Number.isFinite(new Date(observedAt).getTime())) {
    throw Object.assign(new Error('Invalid plan identity or timestamp'), { status: 400 });
  }
  const root = Array.isArray(input) ? input[0] : input;
  if (!root || typeof root !== 'object' || !root.Plan || typeof root.Plan !== 'object') {
    throw Object.assign(new Error('Expected PostgreSQL EXPLAIN FORMAT JSON plan'), { status: 400 });
  }
  const nodes = [];
  const shape = [];
  function walk(node, depth) {
    if (depth > 12 || nodes.length >= 100 || typeof node !== 'object' || node === null) {
      throw Object.assign(new Error('Plan exceeds size or depth limit'), { status: 400 });
    }
    const type = node['Node Type'];
    if (typeof type !== 'string' || !/^[A-Za-z ]{2,80}$/.test(type)) {
      throw Object.assign(new Error('Invalid plan node'), { status: 400 });
    }
    nodes.push({ depth, type, totalCost: numeric(node['Total Cost']),
      planRows: numeric(node['Plan Rows']), actualTotalMs: numeric(node['Actual Total Time']),
      actualRows: numeric(node['Actual Rows']), actualLoops: numeric(node['Actual Loops']),
      sharedHitBlocks: numeric(node['Shared Hit Blocks']),
      sharedReadBlocks: numeric(node['Shared Read Blocks']),
      tempWrittenBlocks: numeric(node['Temp Written Blocks']) });
    shape.push(`${depth}:${type}`);
    if (node.Plans !== undefined && !Array.isArray(node.Plans)) {
      throw Object.assign(new Error('Invalid plan child nodes'), { status: 400 });
    }
    for (const child of node.Plans ?? []) walk(child, depth + 1);
  }
  walk(root.Plan, 0);
  return { id: randomUUID(), queryId: String(queryId),
    observedAt: new Date(observedAt).toISOString(), source: 'operator_import',
    shapeHash: createHash('sha256').update(shape.join('|')).digest('hex'),
    summary: { nodeCount: nodes.length, planningMs: numeric(root['Planning Time']),
      executionMs: numeric(root['Execution Time']), nodes } };
}

export function comparePlans(plans) {
  return plans.map((plan, index) => {
    const prior = plans.slice(index + 1).find(candidate => candidate.queryId === plan.queryId);
    const currentMs = plan.summary.executionMs;
    const priorMs = prior?.summary.executionMs;
    return { ...plan, comparison: prior ? { priorId: prior.id,
      shapeChanged: prior.shapeHash !== plan.shapeHash,
      executionRatio: currentMs > 0 && priorMs > 0 ? currentMs / priorMs : null } : null };
  });
}
