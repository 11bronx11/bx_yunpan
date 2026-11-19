// Sample cumulative bytes on a clock, independently of XHR and confirmation
// callback frequency. Each upload attempt owns its own meter.
export const createUploadSpeedMeter = ({ now = () => performance.now(), windowMs = 2000 } = {}) => {
  let total = 0;
  const samples = [];
  return {
    update(bytes) {
      if (!Number.isFinite(bytes) || bytes < 0) return;
      if (!samples.length) samples.push({ time: now(), bytes: 0 });
      total = Math.max(total, bytes);
    },
    read() {
      if (!samples.length) return 0;
      const time = now();
      if (time > samples[samples.length - 1].time) samples.push({ time, bytes: total });
      while (samples.length > 1 && samples[1].time <= time - windowMs) samples.shift();
      const first = samples[0];
      const elapsed = time - first.time;
      return elapsed > 0 ? Math.round((total - first.bytes) * 1000 / elapsed) : 0;
    },
  };
};
