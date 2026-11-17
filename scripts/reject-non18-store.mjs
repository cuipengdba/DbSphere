import assert from 'node:assert/strict';
import { PgStore } from '../src/pg-store.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
await assert.rejects(PgStore.create(process.env.DBSPHERE_DATABASE_URL),
  /DbSphere long-term store requires PostgreSQL 18/);
console.log('Non-PostgreSQL-18 long-term store was rejected');
