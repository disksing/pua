<script lang="ts">
  import { onMount } from "svelte";
  import { waveformPoints } from "./companion/model";

  let { pulses, live }: { pulses: any[]; live: boolean } = $props();
  let now = $state(Date.now());
  const points = $derived((waveformPoints as any)(pulses, now, 700, 86, live));

  onMount(() => {
    let frame = 0;
    const tick = () => { now = Date.now(); frame = window.requestAnimationFrame(tick); };
    frame = window.requestAnimationFrame(tick);
    return () => window.cancelAnimationFrame(frame);
  });
</script>

<div class="companion-ecg" data-real-pulse-count={pulses.length}>
  <svg viewBox="0 0 700 86" preserveAspectRatio="none" aria-label="Live activity waveform">
    <g class="companion-ecg-grid"><line x1="0" y1="28" x2="700" y2="28" /><line x1="0" y1="57" x2="700" y2="57" /></g>
    <polyline class="companion-ecg-line" {points} />
  </svg>
  {#if live}<span class="companion-scanbar"></span>{/if}
</div>
