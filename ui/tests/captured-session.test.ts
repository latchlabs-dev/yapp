import { test } from 'node:test';
import assert from 'node:assert/strict';
import { belongsToCapturedSession } from '../src/lib/captured-session.ts';

const scope = {conversation_id:'same',tenant_id:'tenant',user_id:'alice',from:'2026-10-19T08:00:00Z',to:'2026-10-19T08:10:00Z'};
const capture = {conversation_id:'same',tenant_id:'tenant',user_id:'alice',created_at:scope.from};
test('shared conversation IDs retain owner, tenant and idle-window boundaries', () => {
  assert.equal(belongsToCapturedSession(capture,scope),true);
  assert.equal(belongsToCapturedSession({...capture,user_id:'bob'},scope),false);
  assert.equal(belongsToCapturedSession({...capture,tenant_id:'other'},scope),false);
  assert.equal(belongsToCapturedSession({...capture,created_at:'2026-10-19T09:00:00Z'},scope),false);
  assert.equal(belongsToCapturedSession({...capture,created_at:scope.to},scope),true);
});
test('key-owned sessions and anonymous captures do not broaden into other owners', () => {
  const keyScope = {...scope,user_id:'',owner_id:'key:first'};
  const keyCapture = {...capture,user_id:'',key_id:'first'};
  assert.equal(belongsToCapturedSession(keyCapture,keyScope),true);
  assert.equal(belongsToCapturedSession({...keyCapture,key_id:'second'},keyScope),false);
  assert.equal(belongsToCapturedSession(keyCapture,{...keyScope,owner_id:''}),false);
  assert.equal(belongsToCapturedSession(capture,{from:scope.from,to:scope.to}),false);
});
