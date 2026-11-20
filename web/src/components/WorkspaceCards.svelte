<script lang="ts">
  import {onMount, untrack} from "svelte";
  import type {WorkspaceTree} from "../models/workspace";
  import CardContent from "./CardContent.svelte";
  import {cardObservation, labels, project, sources, styles, summarize, type Card, type Kind, type Sample, type Summary} from "./workspace-cards";

  let {workspace,user,tree,treeError,onSelect,onClose}: {
    workspace:string; user:string; tree:WorkspaceTree|null; treeError:string;
    onSelect:(id:string)=>Promise<void>; onClose:()=>void;
  } = $props();
  let cards=$state<Card[]>([]);
  let samples=$state<Sample[]>([]);
  let adding=$state(false), loaded=$state(false), busy=$state(false), error=$state("");
  let kind=$state<Kind>("stat"), source=$state("running"), title=$state("");
  let dragging=$state("");
  let offline=$state(false);
  const summary=$derived(tree && !treeError ? summarize(tree) : null);
  const cacheKey=$derived(`pua-card-samples:${JSON.stringify([workspace,user])}`);
  let serial=0, disposed=false;
  const abort=new AbortController();
  async function request(body?:object) {
    const response=await fetch(`/api/workspaces/${encodeURIComponent(workspace)}/cards`,{
      method:body ? "POST":"GET", signal:abort.signal,
      headers:{"X-PUA-User":user,"Content-Type":"application/json"},body:body ? JSON.stringify(body):undefined,
    });
    if (!response.ok) throw new Error((await response.json()).error || `Cards: ${response.status}`);
    return response.json();
  }
  async function load() {
    if (busy || disposed) return;
    const current=++serial;
    try {
      const next=await request();
      if (current!==serial || disposed) return;
      cards=next; loaded=true; error="";
    } catch(reason) {if(current===serial && !disposed) error=String(reason);}
  }
  async function change(body:object) {
    if (busy) return;
    busy=true; serial++;
    try {
      const result=await request(body);
      if(disposed) return;
      cards=result.cards; error=""; return true;
    } catch(reason) {if(!disposed) error=String(reason);}
    finally {busy=false;}
  }
  function layout(card:Card, patch:object) {
    return change({action:"layout",id:card.id,version:card.version,...patch});
  }
  async function add() {
    if(await change({action:"create",kind,source,title:title.trim() || labels[source]})) {adding=false;title="";}
  }
  onMount(() => {
    try {
      const stored=JSON.parse(sessionStorage.getItem(cacheKey) ?? "[]");
      if(Array.isArray(stored)) samples=stored.filter(item=>item && Number.isFinite(Date.parse(item.time)) && ["tasks","running","running-tasks"].every(source=>Number.isFinite(item.counts?.[source]))).slice(-120);
    } catch {samples=[];}
    const unsubscribe=cardObservation.subscribe(observation=>{
      if(observation?.workspace!==workspace || observation.user!==user) return;
      offline=!observation.tree;
      if(observation.tree) sample(summarize(observation.tree),cacheKey);
    });
    void load();
    const timer=setInterval(load,4000);
    return ()=>{disposed=true;abort.abort();clearInterval(timer);unsubscribe();};
  });
  $effect(()=>{
    const current=summary, key=cacheKey;
    if(!current) return;
    untrack(()=>sample(current,key));
  });
  function sample(current:Summary,key:string) {
      const counts=Object.fromEntries(Object.entries(current.bySource).map(([key,items])=>[key,items.length]));
      const last=samples.at(-1), now=Date.now();
      if(last && now-Date.parse(last.time)<4000 && Object.keys(counts).every(key=>counts[key]===last.counts[key])) return;
      samples=[...samples.slice(-119),{time:new Date(now).toISOString(),counts}];
      try {sessionStorage.setItem(key,JSON.stringify(samples));} catch {}
  }
  async function select(id:string) {
    try {await onSelect(id);onClose();} catch(reason){error=String(reason);}
  }
</script>

<div class="board">
  <header><h2>Workspace cards</h2><button class="primary" disabled={!loaded} onclick={()=>adding=!adding} aria-expanded={adding}>+ Add card</button><button class="cards-drawer-close" onclick={onClose} aria-label="Close cards">×</button></header>
  {#if adding}
    <form onsubmit={(event)=>{event.preventDefault();void add();}}>
      <label>Style<select value={kind} onchange={(event)=>{kind=event.currentTarget.value as Kind;source=sources[kind][0];}}>
        {#each Object.entries(styles).filter(([key])=>key!=="html") as [key,name]}<option value={key}>{name}</option>{/each}
      </select></label>
      <label>Data source<select bind:value={source}>{#each sources[kind] as key}<option value={key}>{labels[key]}</option>{/each}</select></label>
      <label>Title<input bind:value={title} maxlength="150" placeholder={labels[source]} /></label>
      <div><button class="primary" disabled={busy}>Add to workspace</button> <button type="button" onclick={()=>adding=false}>Cancel</button></div>
    </form>
  {/if}
  {#if error}<p role="alert">{error} <button onclick={load}>Retry</button></p>{/if}
  <div class="stack">
    {#each cards as card,index (card.id)}
      {@const data=project(card,offline ? null:summary,samples)}
      <article draggable="true" ondragstart={()=>dragging=card.id} ondragend={()=>dragging=""}
        ondragover={(event)=>event.preventDefault()} ondrop={(event)=>{event.preventDefault();const item=cards.find(item=>item.id===dragging);if(item && item.id!==card.id) void layout(item,{before:card.id});dragging="";}}>
        <div class="heading">
          <button class="name" aria-expanded={!card.collapsed} onclick={()=>layout(card,{collapsed:!card.collapsed})}><span aria-hidden="true">{card.collapsed ? "▸":"▾"}</span> {card.title}</button>
          <details><summary aria-label={`Options for ${card.title}`}>···</summary><div class="menu">
            <button disabled={index===0||busy} onclick={()=>layout(card,{before:cards[index-1].id})}>Move up</button>
            <button disabled={index===cards.length-1||busy} onclick={()=>layout(card,{before:cards[index+2]?.id ?? ""})}>Move down</button>
            <button onclick={()=>layout(card,{height:card.height===180 ? 320:card.height===320 ? 0:180})}>Resize</button>
            <button onclick={()=>change({action:"remove",id:card.id,version:card.version})}>Remove</button>
          </div></details>
        </div>
        {#if !card.collapsed}
          <div class="body" style:height={card.height ? `${card.height}px`:undefined}>
            {#if data}<CardContent kind={card.kind} {data} onSelect={select}/>{:else}<small>Data unavailable</small>{/if}
          </div>
          <footer>{styles[card.kind]} · {labels[card.source]}{#if card.updated} · {new Date(card.updated).toLocaleTimeString()}{/if}</footer>
        {/if}
      </article>
    {:else}{#if loaded}<p>Add a card to follow this workspace.</p>{/if}{/each}
  </div>
</div>
<style>
  .board{min-width:0;overflow:auto;height:100%;padding:14px;background:var(--bg);color:var(--text)}
  header,.heading{display:flex;align-items:center;gap:8px}header{margin-bottom:14px}h2{font-size:1rem;margin:0;flex:1}
  button,summary{cursor:pointer}button:disabled{opacity:.5;cursor:default}
  button,select,input{font:inherit;color:inherit}button,select,input,form,article,.menu{border:1px solid var(--border,#ccd3d8);border-radius:6px;background:var(--panel,#fff)}
  button{padding:4px 8px}.primary{background:var(--text,#263746);color:var(--panel,#fff);border-color:transparent}
  .stack{display:grid;gap:10px}form,article{padding:12px}form{display:grid;gap:10px;margin-bottom:12px}
  label{display:grid;gap:4px;font-size:.8rem}select,input{padding:6px;width:100%;box-sizing:border-box}
  .name{border:0;text-align:left;padding:0;font-weight:650;flex:1;overflow-wrap:anywhere}.name span{opacity:.6}
  .heading{justify-content:space-between}.body{margin-top:12px;overflow:auto}.menu{position:absolute;right:0;top:100%;z-index:2;padding:4px;min-width:100px;box-shadow:0 5px 18px #0002}
  details{position:relative}summary{list-style:none;padding:2px 6px;font-weight:bold}.menu button{display:block;border:0;width:100%;text-align:left}
  footer,small,.stack>p{font-size:.75rem;color:var(--muted,#657481)}footer{margin-top:9px}p[role=alert]{overflow-wrap:anywhere}
</style>
