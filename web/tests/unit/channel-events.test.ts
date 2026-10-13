import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import ChannelEvents from "../../src/components/ChannelEvents.svelte";
import { MemoryStorage } from "../fixtures/memory-storage";
import DetailPanel from "../../src/components/DetailPanel.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); document.body.replaceChildren(); FakeEventSource.instances = []; });
class FakeEventSource {
 static instances: FakeEventSource[] = [];
 onmessage: ((event: MessageEvent) => void) | null = null;
 onerror = null;
 readyState = 1;
 closed = false;
 constructor(readonly url: string) { FakeEventSource.instances.push(this); }
 addEventListener() {}
 close() { this.closed = true; }
 emit(id: number, type: string, data: object) {
  const channel = this.url.includes('/chat/runtime') ? 'dialogue' : 'execution';
  const sessionId = `session-${channel}`;
  this.onmessage?.(new MessageEvent('message', { data: JSON.stringify({schema:'agenthub.semantic-events.v1',cursor:id,mode:'replace',source:{eventId:id,type,sessionId,turnId:'turn-1'},events:[{id:`sem_${id}_0`,sourceEventId:id,index:0,type,sessionId,turnId:'turn-1',data}]}) }));
 }
}
function response(data: unknown) { return new Response(JSON.stringify(data), { headers: { 'Content-Type':'application/json' } }); }
function installFetch() {
 const requests: {path: string; init?: RequestInit}[] = [];
 vi.stubGlobal('EventSource', FakeEventSource);
 vi.stubGlobal('fetch', vi.fn(async (input, init) => {
  const path = String(input); requests.push({path,init});
  const channel = path.includes('/chat/runtime') ? 'dialogue' : 'execution';
  const generation = {generation:1,generationId:`gen-${channel}`,status:'running',createdAt:'2026-10-13T00:00:00Z'};
  const turn = {reference:`ref-${channel}`,turnId:'turn-1',closed:false,status:'active',startEventId:1,lastEventId:2,triggerPreview:`${channel} trigger`,generation};
  if (path.endsWith('/chat')) return response({binding:{kind:'profile',name:'default'}});
  if (path.endsWith('/status')) return response({resourceId:'project1.task1',generation,sessionState:'working',session:{id:`session-${channel}`,state:'running',currentTurnId:'turn-1'}});
  if (path.includes('/history/turns/ref-')) return response({turn,latestEventId:2,items:[{type:'message',role:'assistant',text:`${channel} history`,startEventId:1,endEventId:2}]});
  if (path.includes('/history/turns?')) return response({segments:[{generation,turns:[turn]}],page:{hasMore:false}});
  throw new Error(`unexpected request ${path}`);
 }));
 return requests;
}
function setup(dialogue: boolean) {
 const model = {...createPUAAppChannels().detail.current(),identity:'w:project1.task1',workspaceId:'w',resourceId:'project1.task1',currentUserName:'disksing'};
 const target=document.body.appendChild(document.createElement('div'));
 const component=mount(ChannelEvents,{target,props:{model,dialogue,onOpenFile:vi.fn(),onOpenLegacy:vi.fn()}});
 const dispose = async () => { await unmount(component); };
 cleanups.push(dispose);
 return {target,dispose};
}
it('renders both original timelines live, read-only, through isolated transports',async()=>{
 const requests=installFetch();
 const execution=setup(false), dialogue=setup(true);
 await vi.waitFor(()=>{
  expect(execution.target.textContent).toContain('execution history');
  expect(dialogue.target.textContent).toContain('dialogue history');
  expect(FakeEventSource.instances).toHaveLength(2);
 });
 const executionStream=FakeEventSource.instances.find(s=>!s.url.includes('/chat/runtime'))!;
 const dialogueStream=FakeEventSource.instances.find(s=>s.url.includes('/chat/runtime'))!;
 expect(new URL(dialogueStream.url,'http://localhost').searchParams.get('user')).toBe('disksing');
 expect(executionStream.url).not.toContain('user=');
 dialogueStream.emit(3,'message.delta',{text:'dialogue live reply',role:'assistant'});
 executionStream.emit(3,'tool.call',{schemaVersion:1,callId:'tool-1',operation:'start',toolKind:'command',name:'Command',summary:'echo execution',status:'running'});
 await vi.waitFor(()=>{
  expect(dialogue.target.textContent).toContain('dialogue live reply');
  expect(execution.target.querySelector('.agent-activity-group')).not.toBeNull();
 });
 expect(execution.target.textContent).not.toContain('dialogue live reply');
 expect(dialogue.target.textContent).not.toContain('execution history');
 for(const target of [execution.target,dialogue.target]) {
  expect(target.querySelector('[data-component-owner="event-timeline"]')).not.toBeNull();
  expect(target.querySelector('textarea,input')).toBeNull();
  expect(target.querySelector('.history-turn')).toBeNull();
 }
 expect(requests.every(({init})=>!init?.method || init.method==='GET')).toBe(true);
 expect(requests.filter(({path})=>path.includes('/chat/runtime')).every(({init})=>new Headers(init?.headers).get('X-PUA-User')==='disksing')).toBe(true);
});
it('closes subscriptions on tab replacement and ignores frames from the old pane',async()=>{
 const requests=installFetch();
 const execution=setup(false);
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(1));
 const old=FakeEventSource.instances[0];
 await execution.dispose();cleanups.pop();
 expect(old.closed).toBe(true);
 const dialogue=setup(true);
 await vi.waitFor(()=>expect(dialogue.target.textContent).toContain('dialogue history'));
 const requestCount=requests.length;
 old.emit(9,'message.delta',{text:'stale execution output',role:'assistant'});
 await tick();
 expect(dialogue.target.textContent).not.toContain('stale execution output');
 expect(requests).toHaveLength(requestCount);
 await dialogue.dispose();cleanups.pop();
 expect(FakeEventSource.instances.every(s=>s.closed)).toBe(true);
});

it('cleans up the real detail tabs when switching resource, user or channel',async()=>{
 installFetch();
 vi.stubGlobal("localStorage", new MemoryStorage());
 const channel = createPUAAppChannels().detail;
 channel.publish({...channel.current(),identity:'w:project1.task1',workspaceId:'w',resourceId:'project1.task1',resourceType:'task',currentUserName:'disksing',loading:false,
  detail:{id:'project1.task1',type:'task',title:'Task',path:'project1/task1',files:[],artifacts:[],repos:[]}});
 const target=document.body.appendChild(document.createElement('div'));
 const component=mount(DetailPanel,{target,props:{channel}});
 cleanups.push(()=>unmount(component));
 await tick();
 const tab = (name: string) => [...target.querySelectorAll<HTMLButtonElement>('[role="tab"]')].find(b=>b.textContent?.trim()===name)!;
 tab('Execution events').click();
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(1));
 const first=FakeEventSource.instances[0];
 tab('Dialog events').click();
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(2));
 expect(first.closed).toBe(true);
 const second=FakeEventSource.instances[1];
 channel.publish({...channel.current(),currentUserName:'other'});
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(3));
 expect(second.closed).toBe(true);
 expect(FakeEventSource.instances[2].url).toContain('user=other');
 const third=FakeEventSource.instances[2];
 channel.publish({...channel.current(),identity:'w:project1.task2',resourceId:'project1.task2',detail:{...channel.current().detail!,id:'project1.task2'}});
 await tick();
 // The selected tab may reset for a new resource; either way the old stream
 // must close before a stream for the next resource can be used.
 expect(third.closed).toBe(true);
 tab('Dialog events').click();
 await vi.waitFor(()=>expect(FakeEventSource.instances.at(-1)?.url).toContain('/project1.task2/chat/runtime/'));
 first.emit(99,'message.delta',{text:'late old-resource event',role:'assistant'});
 await tick();
 expect(target.textContent).not.toContain('late old-resource event');
});
