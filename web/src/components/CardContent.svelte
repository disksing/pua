<script lang="ts">
  import type { Content, Kind, Point } from "./workspace-cards";
  let {kind, data, onSelect}: {kind: Kind; data: Content; onSelect: (id:string)=>Promise<void>} = $props();
  const palette = ["#437d9a", "#e39b54", "#8a78b2", "#509780", "#bd6e7a", "#888b5e", "#788798"];
  let items = $derived(data.items ?? []);
  let total = $derived(items.reduce((sum,item) => sum + (item.value ?? 0), 0));
  let gradient = $derived.by(() => {
    let current = 0;
    return items.map((item,index) => {
      const start = current;
      current += (item.value ?? 0) * 100 / total;
      return `${palette[index % palette.length]} ${start}% ${current}%`;
    }).join(",");
  });
  const percent = $derived(data.max ? Math.round((data.value ?? 0) * 100 / data.max) : null);
  function coordinates(points: Point[]) {
    const times = points.map(point => Date.parse(point.time));
    const values = points.map(point => point.value);
    const low = Math.min(...values), high = Math.max(...values);
    return points.map((point,index) => ({
      x: 3 + (times[index]-times[0]) / (times.at(-1)!-times[0] || 1) * 254,
      y: high === low ? 27 : 49-(point.value-low)/(high-low)*44,
      gap: index > 0 && times[index]-times[index-1] > 60000,
    }));
  }
  const dots = $derived(coordinates(data.points ?? []));
  const line = $derived(dots.map((dot,index) => `${index===0 || dot.gap ? "M":"L"}${dot.x} ${dot.y}`).join(" "));
</script>

{#if kind === "stat" || kind === "trend"}
  <strong class="number">{data.value ?? data.points?.at(-1)?.value ?? "—"}</strong>
  {#if dots.length}
    <svg viewBox="0 0 260 54" preserveAspectRatio="none" role="img" aria-label="Observed values over time">
      <path d={line} fill="none" stroke="currentColor" stroke-width="2" vector-effect="non-scaling-stroke" />
      {#each dots as dot,index}{#if index===0 || dot.gap || index===dots.length-1}<circle cx={dot.x} cy={dot.y} r="2" fill="currentColor" />{/if}{/each}
    </svg>
    {#if kind === "trend"}<div class="range"><span>{new Date(data.points![0].time).toLocaleTimeString()}</span><span>{new Date(data.points!.at(-1)!.time).toLocaleTimeString()}</span></div>{/if}
  {:else if kind === "trend"}<small>Waiting for samples</small>{/if}
{:else if kind === "gauge"}
  {#if percent === null}<small>No tasks</small>{:else}
    <div class="gauge"><strong class="number">{percent}%</strong><small>{data.value} / {data.max}</small></div>
    <meter min="0" max={data.max} value={data.value} aria-label="Gauge value"></meter>
  {/if}
{:else if kind === "pie"}
  {#if total > 0}
    <div class="distribution"><div class="pie" style:background={`conic-gradient(${gradient})`}></div>
      <ul>{#each items as item,index}<li><i style:background={palette[index % palette.length]}></i>{item.label.replaceAll("_"," ")} <b>{item.value ?? 0}</b></li>{/each}</ul>
    </div>
  {:else}<small>No distribution yet</small>{/if}
{:else if kind === "list"}
  <ul>{#each items as item}<li>{#if item.resource}<button onclick={() => onSelect(item.resource!)}>{item.label}</button>
    {:else if item.url}<a href={item.url} target="_blank" rel="noopener noreferrer">{item.label}</a>{:else}{item.label}{/if}</li>
    {:else}<li><small>No items</small></li>{/each}</ul>
{:else if kind === "html"}
  <iframe title="Published card content" sandbox="" referrerpolicy="no-referrer" srcdoc={`<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; form-action 'none'; base-uri 'none'"><style>body{font:14px/1.5 system-ui;margin:0;color:#252c35}h1,h2,h3,p,ul{margin:.3em 0}table{border-collapse:collapse;width:100%}th,td{border-bottom:1px solid #ddd;padding:4px;text-align:left}</style>${data.html ?? ""}`}></iframe>
{/if}
<style>
  .number{font-size:2rem;line-height:1.15;font-variant-numeric:tabular-nums}small,.range{opacity:.7;font-size:.75rem}
  svg{display:block;width:100%;height:50px;color:#437d9a}.range,.gauge{display:flex;align-items:baseline;justify-content:space-between}
  meter{width:100%;height:16px}.distribution{display:flex;align-items:center;gap:16px}.pie{width:76px;aspect-ratio:1;border-radius:50%;flex-shrink:0}
  ul{padding:0;margin:0;list-style:none}li{padding:4px 0;overflow-wrap:anywhere}i{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
  li button,li a{background:none;border:0;padding:0;color:var(--accent,#37799d);text-align:left;cursor:pointer;text-decoration:underline}iframe{width:100%;border:0;min-height:8em;background:white}
</style>
