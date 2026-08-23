import { startDownload } from './download';

test('downloads in the current page without opening a new window', () => {
  const click = jest.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});

  startDownload('https://example.test/report.pdf', 'report.pdf');

  expect(click).toHaveBeenCalledTimes(1);
  const link = click.mock.instances[0];
  expect(link.href).toBe('https://example.test/report.pdf');
  expect(link.download).toBe('report.pdf');
  expect(link.target).toBe('');
  expect(document.body).not.toContainElement(link);

  click.mockRestore();
});
