import { createUploadSpeedMeter } from './uploadSpeed';

test('uses a moving average without treating confirmation callbacks as new transfers', () => {
  let time = 0;
  const meter = createUploadSpeedMeter({ now: () => time });
  meter.update(0);
  for (let tick = 1; tick <= 8; tick += 1) {
    time = tick * 500;
    meter.update(tick * 500);
    expect(meter.read()).toBe(1000);
    meter.update(tick * 500);
    meter.update(tick * 500);
    expect(meter.read()).toBe(1000);
  }
});

test('the average reaches zero after progress stops', () => {
  let time = 0;
  const meter = createUploadSpeedMeter({ now: () => time });
  meter.update(0);
  time = 500;
  meter.update(1000);
  expect(meter.read()).toBe(2000);
  for (time = 1000; time <= 2500; time += 500) meter.read();
  expect(meter.read()).toBe(0);
});

test('hashing before the first transfer does not lower its measured speed', () => {
  let time = 0;
  const meter = createUploadSpeedMeter({ now: () => time });
  time = 30000;
  meter.update(0);
  time = 31000;
  meter.update(1000);
  expect(meter.read()).toBe(1000);
});

test('concurrent uploads and a resumed attempt have independent byte counters', () => {
  let time = 0;
  const first = createUploadSpeedMeter({ now: () => time });
  const second = createUploadSpeedMeter({ now: () => time });
  first.update(0);
  second.update(0);
  time = 1000;
  first.update(1000);
  second.update(2000);
  expect(first.read()).toBe(1000);
  expect(second.read()).toBe(2000);
  const resumed = createUploadSpeedMeter({ now: () => time });
  resumed.update(0);
  time = 2000;
  resumed.update(500);
  expect(resumed.read()).toBe(500);
});
