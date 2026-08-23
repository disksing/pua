<script lang="ts">
  import { onMount } from "svelte";
  import { ACTIVITY_VISIBLE_MS, waveformSampleY } from "./companion/model";

  let { pulses, live }: { pulses: any[]; live: boolean } = $props();
  let canvas: HTMLCanvasElement;
  let syncPulses = () => {};

  $effect(() => {
    pulses;
    live;
    syncPulses();
  });

  onMount(() => {
    const pulseMarginMs = 500;
    const futureBufferMs = ACTIVITY_VISIBLE_MS;
    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
    let animation: Animation | null = null;
    let timer = 0;
    let disposed = false;
    let anchorTime = Date.now();
    let visibleWidth = 1;
    let trackWidth = 1;
    let height = 1;
    let lastLive = live;
    let knownPulses = new Set<string>();

    const pulseKey = (pulse: any) => `${pulse.sessionId}:${pulse.at}`;
    const stop = () => {
      window.clearTimeout(timer);
      timer = 0;
      animation?.cancel();
      animation = null;
    };

    const drawRange = (fromX: number, toX: number) => {
      const context = canvas.getContext("2d");
      if (!context) return;
      const startX = Math.max(0, Math.floor(fromX) - 6);
      const endX = Math.min(trackWidth, Math.ceil(toX) + 6);
      if (endX <= startX) return;

      context.save();
      context.beginPath();
      context.rect(startX, 0, endX - startX, height);
      context.clip();
      context.clearRect(startX, 0, endX - startX, height);
      context.lineWidth = 1;
      context.strokeStyle = "#1c303c";
      context.beginPath();
      context.moveTo(startX, height / 3);
      context.lineTo(endX, height / 3);
      context.moveTo(startX, height * 2 / 3);
      context.lineTo(endX, height * 2 / 3);
      context.stroke();

      context.lineWidth = 1.7;
      context.lineJoin = "round";
      context.lineCap = "round";
      context.strokeStyle = "#62e6a5";
      context.shadowBlur = 3;
      context.shadowColor = "rgb(98 230 165 / 55%)";
      context.beginPath();
      for (let x = startX; x <= endX; x += 2) {
        const sampleTime = anchorTime - ACTIVITY_VISIBLE_MS + x / visibleWidth * ACTIVITY_VISIBLE_MS;
        const y = (waveformSampleY as any)(pulses, sampleTime, height, live);
        if (x === startX) context.moveTo(x, y);
        else context.lineTo(x, y);
      }
      context.stroke();
      context.restore();
    };

    const drawTrack = (nextAnchor = Date.now()) => {
      if (disposed || document.hidden) return;
      stop();
      const container = canvas.parentElement;
      const context = canvas.getContext("2d");
      if (!container || !context) return;

      visibleWidth = Math.max(1, container.clientWidth);
      height = Math.max(1, container.clientHeight);
      trackWidth = visibleWidth * (1 + futureBufferMs / ACTIVITY_VISIBLE_MS);
      const pixelRatio = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
      canvas.style.width = `${trackWidth}px`;
      canvas.style.height = `${height}px`;
      canvas.width = Math.round(trackWidth * pixelRatio);
      canvas.height = Math.round(height * pixelRatio);
      context.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
      anchorTime = nextAnchor;
      lastLive = live;
      knownPulses = new Set((pulses || []).map(pulseKey));
      drawRange(0, trackWidth);

      if (reducedMotion.matches) {
        timer = window.setTimeout(() => drawTrack(Date.now()), 1000);
        return;
      }
      const nextAnchorTime = anchorTime + futureBufferMs;
      animation = canvas.animate(
        [{ transform: "translateX(0)" }, { transform: `translateX(${-visibleWidth}px)` }],
        { duration: futureBufferMs, easing: "linear", fill: "forwards" },
      );
      animation.onfinish = () => drawTrack(nextAnchorTime);
    };

    const updatePulses = () => {
      if (disposed || document.hidden) return;
      if (live !== lastLive || reducedMotion.matches || !animation) {
        drawTrack(Date.now());
        return;
      }
      const currentKeys = new Set((pulses || []).map(pulseKey));
      const added = (pulses || []).filter((pulse: any) => !knownPulses.has(pulseKey(pulse)));
      knownPulses = currentKeys;
      const ranges: Array<{ from: number; to: number }> = [];
      for (const pulse of added) {
        const fromTime = Number(pulse.at) - pulseMarginMs;
        const toTime = Number(pulse.at) + pulseMarginMs;
        const fromX = (fromTime - (anchorTime - ACTIVITY_VISIBLE_MS)) / ACTIVITY_VISIBLE_MS * visibleWidth;
        const toX = (toTime - (anchorTime - ACTIVITY_VISIBLE_MS)) / ACTIVITY_VISIBLE_MS * visibleWidth;
        if (toX < 0) continue;
        if (fromX > trackWidth) {
          drawTrack(Date.now());
          return;
        }
        ranges.push({ from: Math.max(0, fromX), to: Math.min(trackWidth, toX) });
      }
      ranges.sort((left, right) => left.from - right.from);
      const merged: Array<{ from: number; to: number }> = [];
      for (const range of ranges) {
        const previous = merged[merged.length - 1];
        if (previous && range.from <= previous.to) previous.to = Math.max(previous.to, range.to);
        else merged.push({ ...range });
      }
      for (const range of merged) drawRange(range.from, range.to);
    };

    const onVisibilityChange = () => {
      if (document.hidden) stop();
      else drawTrack(Date.now());
    };
    const onMotionChange = () => drawTrack(Date.now());
    const resize = new ResizeObserver(() => drawTrack(Date.now()));
    resize.observe(canvas.parentElement!);
    reducedMotion.addEventListener("change", onMotionChange);
    document.addEventListener("visibilitychange", onVisibilityChange);
    syncPulses = updatePulses;
    drawTrack();

    return () => {
      disposed = true;
      stop();
      syncPulses = () => {};
      resize.disconnect();
      reducedMotion.removeEventListener("change", onMotionChange);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  });
</script>

<div class="companion-ecg" data-real-pulse-count={pulses.length} role="img" aria-label="Live activity waveform">
  <canvas bind:this={canvas} aria-hidden="true"></canvas>
  {#if live}<span class="companion-scanbar"></span>{/if}
</div>
