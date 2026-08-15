import { bodyFont, palette } from './styles/tokens';

export const theme = {
  token: {
    colorPrimary: palette.lime,
    colorPrimaryText: palette.ink,
    colorPrimaryTextHover: palette.ink,
    colorTextLightSolid: palette.ink,
    colorSuccess: '#16835f',
    colorInfo: palette.blue,
    colorWarning: '#b86b00',
    colorError: '#c7352f',
    colorText: palette.ink,
    colorTextSecondary: palette.muted,
    colorBgBase: palette.paper,
    colorBgContainer: palette.white,
    colorBorder: palette.line,
    borderRadius: 14,
    controlHeight: 40,
    fontFamily: bodyFont,
    boxShadow: '0 1px 2px rgba(0, 0, 0, 0.03), 0 4px 12px rgba(0, 0, 0, 0.035)',
    boxShadowSecondary: '0 8px 24px rgba(0, 0, 0, 0.055)',
  },
  components: {
    Button: { primaryColor: palette.ink, primaryShadow: 'none', defaultShadow: 'none' },
    Card: { headerBg: 'transparent' },
    Modal: { contentBg: palette.white, headerBg: palette.white },
    Table: { headerBg: '#faf9f6', rowHoverBg: 'rgba(198, 255, 0, 0.07)' },
    Tooltip: { colorBgSpotlight: palette.ink },
  },
};
