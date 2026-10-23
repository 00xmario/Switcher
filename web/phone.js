// Switcher on your phone. Served only through the Tailscale add-on to the
// owner's devices, and only to phones approved on the Mac. The page can read
// accounts and usage, refresh them, switch accounts and spend banked resets.
//
// It draws accounts the way the menu bar panel does: one card per provider,
// one row per account, compact usage lines with a short bar, and the active
// account's plan and check mark in the accent colour.
import { switcherMark, playMark } from './brand.js';

const PROVIDERS = { codex: 'Codex', claude: 'Claude', grok: 'Grok', opencode: 'OpenCode', antigravity: 'Antigravity', gemini: 'Gemini', copilot: 'Copilot' };
const PLANS = {
  claude_max_5x: 'Max 5x', claude_max_20x: 'Max 20x', claude_max: 'Max', claude_pro: 'Pro', max: 'Max', pro: 'Pro 20x',
  prolite: 'Pro 5x', plus: 'Plus', go: 'Go', free: 'Free', team: 'Team', business: 'Business', enterprise: 'Enterprise',
  edu: 'Edu', copilot_pro: 'Pro', copilot_pro_plus: 'Pro+', copilot_business: 'Business',
  copilot_enterprise: 'Enterprise', copilot_free: 'Free',
};
const OUTCOMES = {
  reset: 'Banked reset used', already_redeemed: 'That reset was already spent',
  nothing_to_reset: 'Nothing to reset right now; the reset was kept', no_credit: 'No banked reset available',
};
// Usage at or below this share left reads as low, as in the web app.
const LOW = 20;

// Provider marks, the same ones the web app (web/app.js) and the menu draw.
const LOGOS = {
  // Official OpenAI circular mark; black disc + white knot reads on both
  // themes.
  codex: `<svg viewBox="0 0 512 512" aria-hidden="true"><path d="M256 0c141.4 0 256 114.6 256 256S397.4 512 256 512 0 397.4 0 256 114.6 0 256 0"/><path style="fill:#fff" d="M212.8 209.5v-35.7c0-2.4.7-4.1 3.1-5.5l66.2-38.4c8.9-5.1 20.3-7.6 31.2-7.6 41.9 0 68.3 32.3 68.3 66.9 0 2.7 0 6.5-.7 9.6l-69.3-40.5c-3.4-2.1-7.2-2.4-11.3 0zm153.1 126.7v-79.3c0-4.5-1.7-7.6-5.8-10l-87.9-51.1 30.9-17.8c1.7-1 4.5-1 6.2 0l66.6 38.4c18.9 11 31.9 35 31.9 58-.1 27.1-16.5 52.1-41.9 61.8m-171.3-68.4L164 249.7c-2.4-1.4-3.1-3.1-3.1-5.5v-76.5c0-37.4 28.5-65.6 67.3-65.6 15.1 0 29.5 5.1 41.2 14.4l-69 40.2c-4.1 2.4-5.8 5.5-5.8 10zm59.7 34.4-41.5-23.3v-49.4l41.5-23.3 41.2 23.3v49.4zm25.7 104c-15.1 0-29.5-5.1-41.2-14.4l69-40.2c4.1-2.4 5.8-5.5 5.8-10V240.4l30.9 18.2c2.4 1.4 3.1 3.1 3.1 5.5v76.5c.1 37.4-28.8 65.6-67.6 65.6m-81-75.9-66.6-38.4c-18.9-11-31.9-35-31.9-58 0-27.5 16.8-52.2 42.2-61.8v79.6c0 4.5 1.7 7.6 5.8 10l87.5 50.8-30.9 17.8c-1.6 1-4.4 1-6.1 0m-4.1 55.6c-39.5 0-68.3-29.5-68.3-66.2 0-3.4.3-6.9.7-10l69 39.8c4.1 2.4 7.6 2.4 11.7 0l87.5-50.8v35.7c0 2.4-.7 4.1-3.1 5.5l-66.2 38.4c-9 5.2-20.3 7.6-31.3 7.6m85.1 38.8c41.2 0 75.5-29.5 83.1-68.7 38.4-9.6 63.2-45.3 63.2-81.7 0-24-10.3-47-29.2-63.8 1.7-7.2 3.1-14.8 3.1-22 0-48.4-39.5-84.8-84.8-84.8-9.3 0-18.5 1.7-27.5 4.8-15.4-15.4-36.4-25.1-59.7-25.1-41.2 0-75.5 29.5-83.1 68.7-38.4 9.6-63.2 45.3-63.2 81.7 0 24 10.3 47 29.2 63.8-1.7 7.2-3.1 14.8-3.1 22 0 48.4 39.5 84.8 84.8 84.8 9.3 0 18.5-1.7 27.5-4.8 15.5 15.5 36.4 25.1 59.7 25.1"/></svg>`,
  // Official Anthropic starburst in its brand clay color; reads on both
  // themes.
  claude: `<svg viewBox="0 0 248 248" aria-hidden="true"><path d="M52.4285 162.873L98.7844 136.879L99.5485 134.602L98.7844 133.334H96.4921L88.7237 132.862L62.2346 132.153L39.3113 131.207L17.0249 130.026L11.4214 128.844L6.2 121.873L6.7094 118.447L11.4214 115.257L18.171 115.847L33.0711 116.911L55.485 118.447L71.6586 119.392L95.728 121.873H99.5485L100.058 120.337L98.7844 119.392L97.7656 118.447L74.5877 102.732L49.4995 86.1905L36.3823 76.62L29.3779 71.7757L25.8121 67.2858L24.2839 57.3608L30.6515 50.2716L39.3113 50.8623L41.4763 51.4531L50.2636 58.1879L68.9842 72.7209L93.4357 90.6804L97.0015 93.6343L98.4374 92.6652L98.6571 91.9801L97.0015 89.2625L83.757 65.2772L69.621 40.8192L63.2534 30.6579L61.5978 24.632C60.9565 22.1032 60.579 20.0111 60.579 17.4246L67.8381 7.49965L71.9133 6.19995L81.7193 7.49965L85.7946 11.0443L91.9074 24.9865L101.714 46.8451L116.996 76.62L121.453 85.4816L123.873 93.6343L124.764 96.1155H126.292V94.6976L127.566 77.9197L129.858 57.3608L132.15 30.8942L132.915 23.4505L136.608 14.4708L143.994 9.62643L149.725 12.344L154.437 19.0788L153.8 23.4505L150.998 41.6463L145.522 70.1215L141.957 89.2625H143.994L146.414 86.7813L156.093 74.0206L172.266 53.698L179.398 45.6635L187.803 36.802L193.152 32.5484H203.34L210.726 43.6549L207.415 55.1159L196.972 68.3492L188.312 79.5739L175.896 96.2095L168.191 109.585L168.882 110.689L170.738 110.53L198.755 104.504L213.91 101.787L231.994 98.7149L240.144 102.496L241.036 106.395L237.852 114.311L218.495 119.037L195.826 123.645L162.07 131.592L161.696 131.893L162.137 132.547L177.36 133.925L183.855 134.279H199.774L229.447 136.524L237.215 141.605L241.8 147.867L241.036 152.711L229.065 158.737L213.019 154.956L175.45 145.977L162.587 142.787H160.805V143.85L171.502 154.366L191.242 172.089L215.82 195.011L217.094 200.682L213.91 205.172L210.599 204.699L188.949 188.394L180.544 181.069L161.696 165.118H160.422V166.772L164.752 173.152L187.803 207.771L188.949 218.405L187.294 221.832L181.308 223.959L174.813 222.777L161.187 203.754L147.305 182.486L136.098 163.345L134.745 164.2L128.075 235.42L125.019 239.082L117.887 241.8L111.902 237.31L108.718 229.984L111.902 215.452L115.722 196.547L118.779 181.541L121.58 162.873L123.291 156.636L123.14 156.219L121.773 156.449L107.699 175.752L86.304 204.699L69.3663 222.777L65.291 224.431L58.2867 220.768L58.9235 214.27L62.8713 208.48L86.304 178.705L100.44 160.155L109.551 149.507L109.462 147.967L108.959 147.924L46.6977 188.512L35.6182 189.93L30.7788 185.44L31.4156 178.115L33.7079 175.752L52.4285 162.873Z" fill="#D97757"/></svg>`,
  // Official xAI mark, currentColor so it is black on light and white on
  // dark (same geometry as the supplied light and dark files).
  grok: `<svg viewBox="0.36 0.5 33.33 32" fill="currentColor" aria-hidden="true"><path d="M13.2371 21.0407L24.3186 12.8506C24.8619 12.4491 25.6384 12.6057 25.8973 13.2294C27.2597 16.5185 26.651 20.4712 23.9403 23.1851C21.2297 25.8989 17.4581 26.4941 14.0108 25.1386L10.2449 26.8843C15.6463 30.5806 22.2053 29.6665 26.304 25.5601C29.5551 22.3051 30.562 17.8683 29.6205 13.8673L29.629 13.8758C28.2637 7.99809 29.9647 5.64871 33.449 0.844576C33.5314 0.730667 33.6139 0.616757 33.6964 0.5L29.1113 5.09055V5.07631L13.2343 21.0436"/><path d="M10.9503 23.0313C7.07343 19.3235 7.74185 13.5853 11.0498 10.2763C13.4959 7.82722 17.5036 6.82767 21.0021 8.2971L24.7595 6.55998C24.0826 6.07017 23.215 5.54334 22.2195 5.17313C17.7198 3.31926 12.3326 4.24192 8.67479 7.90126C5.15635 11.4239 4.0499 16.8403 5.94992 21.4622C7.36924 24.9165 5.04257 27.3598 2.69884 29.826C1.86829 30.7002 1.0349 31.5745 0.36364 32.5L10.9474 23.0341"/></svg>`,
  // OpenCode mark; the dark frame is inverted in dark theme so it stays
  // visible.
  opencode: `<svg viewBox="0 0 240 300" aria-hidden="true"><path d="M180 240H60V120H180V240Z" fill="#CFCECD"/><path d="M180 60H60V240H180V60ZM240 300H0V0H240V300Z" fill="#211E1E"/></svg>`,
  // Official Antigravity mark, full color so it reads on both themes.
  antigravity: `<svg viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><title>Antigravity</title><mask height="23" id="lobe-icons-antigravity-0-_R_0_" maskUnits="userSpaceOnUse" width="24" x="0" y="1"><path d="M21.751 22.607c1.34 1.005 3.35.335 1.508-1.508C17.73 15.74 18.904 1 12.037 1 5.17 1 6.342 15.74.815 21.1c-2.01 2.009.167 2.511 1.507 1.506 5.192-3.517 4.857-9.714 9.715-9.714 4.857 0 4.522 6.197 9.714 9.715z" fill="#fff"></path></mask><g mask="url(#lobe-icons-antigravity-0-_R_0_)"><g filter="url(#lobe-icons-antigravity-1-_R_0_)"><path d="M-1.018-3.992c-.408 3.591 2.686 6.89 6.91 7.37 4.225.48 7.98-2.043 8.387-5.633.408-3.59-2.686-6.89-6.91-7.37-4.225-.479-7.98 2.043-8.387 5.633z" fill="#FFE432"></path></g><g filter="url(#lobe-icons-antigravity-2-_R_0_)"><path d="M15.269 7.747c1.058 4.557 5.691 7.374 10.348 6.293 4.657-1.082 7.575-5.653 6.516-10.21-1.058-4.556-5.691-7.374-10.348-6.292-4.657 1.082-7.575 5.653-6.516 10.21z" fill="#FC413D"></path></g><g filter="url(#lobe-icons-antigravity-3-_R_0_)"><path d="M-12.443 10.804c1.338 4.703 7.36 7.11 13.453 5.378 6.092-1.733 9.947-6.95 8.61-11.652C8.282-.173 2.26-2.58-3.833-.848-9.925.884-13.78 6.1-12.443 10.804z" fill="#00B95C"></path></g><g filter="url(#lobe-icons-antigravity-4-_R_0_)"><path d="M-12.443 10.804c1.338 4.703 7.36 7.11 13.453 5.378 6.092-1.733 9.947-6.95 8.61-11.652C8.282-.173 2.26-2.58-3.833-.848-9.925.884-13.78 6.1-12.443 10.804z" fill="#00B95C"></path></g><g filter="url(#lobe-icons-antigravity-5-_R_0_)"><path d="M-7.608 14.703c3.352 3.424 9.126 3.208 12.896-.483 3.77-3.69 4.108-9.459.756-12.883C2.69-2.087-3.083-1.871-6.853 1.82c-3.77 3.69-4.108 9.458-.755 12.883z" fill="#00B95C"></path></g><g filter="url(#lobe-icons-antigravity-6-_R_0_)"><path d="M9.932 27.617c1.04 4.482 5.384 7.303 9.7 6.3 4.316-1.002 6.971-5.448 5.93-9.93-1.04-4.483-5.384-7.304-9.7-6.301-4.316 1.002-6.971 5.448-5.93 9.93z" fill="#3186FF"></path></g><g filter="url(#lobe-icons-antigravity-7-_R_0_)"><path d="M2.572-8.185C.392-3.329 2.778 2.472 7.9 4.771c5.122 2.3 11.042.227 13.222-4.63 2.18-4.855-.205-10.656-5.327-12.955-5.122-2.3-11.042-.227-13.222 4.63z" fill="#FBBC04"></path></g><g filter="url(#lobe-icons-antigravity-8-_R_0_)"><path d="M-3.267 38.686c-5.277-2.072 3.742-19.117 5.984-24.83 2.243-5.712 8.34-8.664 13.616-6.592 5.278 2.071 11.533 13.482 9.29 19.195-2.242 5.713-23.613 14.298-28.89 12.227z" fill="#3186FF"></path></g><g filter="url(#lobe-icons-antigravity-9-_R_0_)"><path d="M28.71 17.471c-1.413 1.649-5.1.808-8.236-1.878-3.135-2.687-4.531-6.201-3.118-7.85 1.412-1.649 5.1-.808 8.235 1.878s4.532 6.2 3.119 7.85z" fill="#749BFF"></path></g><g filter="url(#lobe-icons-antigravity-10-_R_0_)"><path d="M18.163 9.077c5.81 3.93 12.502 4.19 14.946.577 2.443-3.612-.287-9.727-6.098-13.658-5.81-3.931-12.502-4.19-14.946-.577-2.443 3.612.287 9.727 6.098 13.658z" fill="#FC413D"></path></g><g filter="url(#lobe-icons-antigravity-11-_R_0_)"><path d="M-.915 2.684c-1.44 3.473-.97 6.967 1.05 7.804 2.02.837 4.824-1.3 6.264-4.772 1.44-3.473.97-6.967-1.05-7.804-2.02-.837-4.824 1.3-6.264 4.772z" fill="#FFEE48"></path></g></g><defs><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="17.587" id="lobe-icons-antigravity-1-_R_0_" width="19.838" x="-3.288" y="-11.917"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="1.117"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="38.565" id="lobe-icons-antigravity-2-_R_0_" width="38.9" x="4.251" y="-13.493"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="5.4"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="36.517" id="lobe-icons-antigravity-3-_R_0_" width="40.955" x="-21.889" y="-10.592"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="4.591"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="36.517" id="lobe-icons-antigravity-4-_R_0_" width="40.955" x="-21.889" y="-10.592"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="4.591"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="36.595" id="lobe-icons-antigravity-5-_R_0_" width="36.632" x="-19.099" y="-10.278"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="4.591"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="34.087" id="lobe-icons-antigravity-6-_R_0_" width="33.533" x=".981" y="8.758"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="4.363"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="35.276" id="lobe-icons-antigravity-7-_R_0_" width="35.978" x="-6.143" y="-21.659"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="3.954"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="46.523" id="lobe-icons-antigravity-8-_R_0_" width="45.114" x="-11.96" y="-.46"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="3.531"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="24.054" id="lobe-icons-antigravity-9-_R_0_" width="25.094" x="10.485" y=".58"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="3.159"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="30.007" id="lobe-icons-antigravity-10-_R_0_" width="33.508" x="5.833" y="-12.467"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="2.669"></feGaussianBlur></filter><filter color-interpolation-filters="sRGB" filterUnits="userSpaceOnUse" height="26.151" id="lobe-icons-antigravity-11-_R_0_" width="22.194" x="-8.355" y="-8.876"><feFlood flood-opacity="0" result="BackgroundImageFix"></feFlood><feBlend in="SourceGraphic" in2="BackgroundImageFix" result="shape"></feBlend><feGaussianBlur result="effect1_foregroundBlur_977_115" stdDeviation="3.303"></feGaussianBlur></filter></defs></svg>`,
  // Official Gemini spark, full color so it reads on both themes.
  gemini: `<svg viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><title>Gemini</title><path d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z" fill="#3186FF"></path><path d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z" fill="url(#lobe-icons-gemini-0-_R_0_)"></path><path d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z" fill="url(#lobe-icons-gemini-1-_R_0_)"></path><path d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z" fill="url(#lobe-icons-gemini-2-_R_0_)"></path><defs><linearGradient gradientUnits="userSpaceOnUse" id="lobe-icons-gemini-0-_R_0_" x1="7" x2="11" y1="15.5" y2="12"><stop stop-color="#08B962"></stop><stop offset="1" stop-color="#08B962" stop-opacity="0"></stop></linearGradient><linearGradient gradientUnits="userSpaceOnUse" id="lobe-icons-gemini-1-_R_0_" x1="8" x2="11.5" y1="5.5" y2="11"><stop stop-color="#F94543"></stop><stop offset="1" stop-color="#F94543" stop-opacity="0"></stop></linearGradient><linearGradient gradientUnits="userSpaceOnUse" id="lobe-icons-gemini-2-_R_0_" x1="3.5" x2="17.5" y1="13.5" y2="12"><stop stop-color="#FABC12"></stop><stop offset=".46" stop-color="#FABC12" stop-opacity="0"></stop></linearGradient></defs></svg>`,
  // Official GitHub Copilot mark, currentColor so it follows the theme.
  copilot: `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M23.922 16.992c-.861 1.495-5.859 5.023-11.922 5.023-6.063 0-11.061-3.528-11.922-5.023A.641.641 0 0 1 0 16.736v-2.869a.841.841 0 0 1 .053-.22c.372-.935 1.347-2.292 2.605-2.656.167-.429.414-1.055.644-1.517a10.195 10.195 0 0 1-.052-1.086c0-1.331.282-2.499 1.132-3.368.397-.406.89-.717 1.474-.952 1.399-1.136 3.392-2.093 6.122-2.093 2.731 0 4.767.957 6.166 2.093.584.235 1.077.546 1.474.952.85.869 1.132 2.037 1.132 3.368 0 .368-.014.733-.052 1.086.23.462.477 1.088.644 1.517 1.258.364 2.233 1.721 2.605 2.656a.832.832 0 0 1 .053.22v2.869a.641.641 0 0 1-.078.256ZM12.172 11h-.344a4.323 4.323 0 0 1-.355.508C10.703 12.455 9.555 13 7.965 13c-1.725 0-2.989-.359-3.782-1.259a2.005 2.005 0 0 1-.085-.104L4 11.741v6.585c1.435.779 4.514 2.179 8 2.179 3.486 0 6.565-1.4 8-2.179v-6.585l-.098-.104s-.033.045-.085.104c-.793.9-2.057 1.259-3.782 1.259-1.59 0-2.738-.545-3.508-1.492a4.323 4.323 0 0 1-.355-.508h-.016.016Zm.641-2.935c.136 1.057.403 1.913.878 2.497.442.544 1.134.938 2.344.938 1.573 0 2.292-.337 2.657-.751.384-.435.558-1.15.558-2.361 0-1.14-.243-1.847-.705-2.319-.477-.488-1.319-.862-2.824-1.025-1.487-.161-2.192.138-2.533.529-.269.307-.437.808-.438 1.578v.021c0 .265.021.562.063.893Zm-1.626 0c.042-.331.063-.628.063-.894v-.02c-.001-.77-.169-1.271-.438-1.578-.341-.391-1.046-.69-2.533-.529-1.505.163-2.347.537-2.824 1.025-.462.472-.705 1.179-.705 2.319 0 1.211.175 1.926.558 2.361.365.414 1.084.751 2.657.751 1.21 0 1.902-.394 2.344-.938.475-.584.742-1.44.878-2.497Z"/><path d="M14.5 14.25a1 1 0 0 1 1 1v2a1 1 0 0 1-2 0v-2a1 1 0 0 1 1-1Zm-5 0a1 1 0 0 1 1 1v2a1 1 0 0 1-2 0v-2a1 1 0 0 1 1-1Z"/></svg>`,
};

const CHECK = '<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M5 12.5l4.5 4.5L19 7.5" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
const PHONE = '<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="6.5" y="2.5" width="11" height="19" rx="2.6"/><path d="M10.5 18.5h3"/></svg>';

export const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

export function duration(seconds) {
  if (!(seconds > 0)) return '';
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${Math.max(m, 1)}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

// windowHTML draws one usage line as what is left: label, a short bar, the
// percentage and the reset countdown. The lines of one account share a grid,
// so their columns line up like the menu's.
export function windowHTML(w, now = Date.now() / 1000) {
  const used = Math.min(100, Math.max(0, Number(w.used_percent) || 0));
  const left = Math.round(100 - used);
  const resets = w.resets_at > now ? duration(w.resets_at - now) : '';
  const low = left <= LOW ? ' is-low' : '';
  return `<span class="line${low}">
      <span class="line-label">${esc(w.label)}:</span>
      <span class="bar" aria-hidden="true"><i style="width:${left}%"></i></span>
      <span class="line-value">${left}% left</span>
      <span class="line-reset">${resets ? `<span aria-hidden="true">↻ </span><span class="sr">, resets in </span>${resets}` : ''}</span>
    </span>`;
}

// accountHTML draws one account row. A row that is not in use is one big
// button that switches to it; a banked reset stays its own button.
export function accountHTML(a, now = Date.now() / 1000) {
  const windows = a.usage?.windows || [];
  const credits = a.reset_credits?.count || 0;
  const out = a.exhausted_until > now;
  const plan = PLANS[a.plan] || a.plan || '';
  const title = `<span class="title">
      <span class="email">${esc(a.email || a.id)}</span>${plan ? `<span class="plan">· ${esc(plan)}</span>` : ''}
      ${credits > 0 ? `<span class="banked">⚡ ${credits} banked</span>` : ''}
      ${a.active ? `<span class="check" title="In use">${CHECK}<span class="sr">In use</span></span>` : '<span class="use">Use</span>'}
    </span>`;
  const notes = [
    out ? `<span class="note is-out">Out of usage · back in ${duration(a.exhausted_until - now)}</span>` : '',
    a.native_switch_available && !a.active ? '<span class="note">Use also switches Claude Code on your Mac.</span>' : '',
  ].join('');
  const usage = windows.length
    ? `<span class="lines">${windows.map(w => windowHTML(w, now)).join('')}</span>`
    : '<span class="note">No usage reported yet</span>';
  const body = `${title}${notes}${usage}`;
  const main = a.active
    ? `<div class="row-main">${body}</div>`
    : `<button type="button" class="row-main" data-use="${esc(a.id)}">${body}</button>`;
  const reset = credits > 0 && a.reset_credits?.next_id
    ? `<div class="row-actions"><button type="button" class="button${out ? ' accent' : ''}" data-reset="${esc(a.id)}">Use banked reset</button></div>`
    : '';
  return `<article class="account${a.active ? ' is-active' : ''}${out ? ' is-out' : ''}" data-account="${esc(a.id)}">${main}${reset}</article>`;
}

// providerHeadHTML is the menu's section head: the provider's mark and name.
function providerHeadHTML(p) {
  return `<h2 class="provider-head"><span class="logo logo-${esc(p)}" aria-hidden="true">${LOGOS[p] || ''}</span>${esc(PROVIDERS[p] || p)}</h2>`;
}

// stateHTML groups accounts by provider in the dashboard's order, one card
// per provider. Accounts keep their order, as in the menu and the web app.
export function stateHTML(state, now = Date.now() / 1000) {
  const hidden = new Set(state.hidden || []);
  const order = [...(state.order || [])];
  for (const a of state.accounts || []) if (!order.includes(a.provider)) order.push(a.provider);
  const groups = order.filter(p => !hidden.has(p)).map(p => {
    const accounts = (state.accounts || []).filter(a => a.provider === p);
    if (!accounts.length) return '';
    return `<section class="provider" data-provider="${esc(p)}">${providerHeadHTML(p)}<div class="card">${accounts.map(a => accountHTML(a, now)).join('')}</div></section>`;
  }).join('');
  return groups || '<div class="panel"><p>No accounts yet. Add them in Switcher on your Mac.</p></div>';
}

export function footerHTML(device) {
  return `<footer class="foot">
    <button type="button" class="button wide" id="sign-out">Sign this phone out</button>
    <p>${device ? `This phone: ${esc(device)}` : 'This phone'} · only your Tailscale devices reach this page</p>
  </footer>`;
}

// codeHTML shows the pairing code in two groups of three, like the boxes the
// Mac asks it to be typed into.
function codeHTML(code) {
  const digits = code.split('');
  const group = part => `<span class="code-group">${part.map(d => `<b>${esc(d)}</b>`).join('')}</span>`;
  return `<div class="code" role="img" aria-label="Code ${esc(digits.join(' '))}">${group(digits.slice(0, 3))}<i class="code-dash"></i>${group(digits.slice(3))}</div>`;
}

export function pairHTML(pairing, now = Date.now() / 1000) {
  const tile = `<span class="tile tile-phone">${PHONE}</span>`;
  if (!pairing) {
    return `<section class="panel">
      ${tile}
      <h1>Use Switcher on this phone</h1>
      <p>Your Mac approves each phone once. This phone will show a code; you type it into Switcher on your Mac.</p>
      <button type="button" class="button primary wide" id="pair-start">Pair this phone</button>
    </section>`;
  }
  const code = String(pairing.code || '');
  const left = Math.max(0, Math.round(pairing.expires_at - now));
  return `<section class="panel">
    ${tile}
    <h1>Approve this phone</h1>
    <p>On your Mac, open Switcher, go to <strong>Settings → Phone</strong> and enter this code.</p>
    ${codeHTML(code)}
    <p class="small">Expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}. Or in a terminal on the Mac:</p>
    <code class="command">switcher phone approve ${esc(code)}</code>
    <div class="waiting"><span class="spinner" aria-hidden="true"></span>Waiting for your Mac</div>
  </section>`;
}

export function messageHTML(title, text) {
  return `<section class="panel">
    <span class="tile">${switcherMark({ tile: true, size: 44, id: 'msg-mark' })}</span>
    <h1>${esc(title)}</h1><p>${esc(text)}</p>
    <button type="button" class="button wide" id="retry">Try again</button>
  </section>`;
}

/* ---------- the page ---------- */

const app = { csrf: '', state: null, pairing: null, timer: 0, mac: '', device: '', busy: false };

class HttpError extends Error {
  constructor(status, body) { super(body?.error || `HTTP ${status}`); this.status = status; this.body = body; }
}

async function api(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (method !== 'GET') {
    headers['X-Switcher-CSRF'] = app.csrf;
    headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store' });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new HttpError(response.status, data);
  return data;
}

const $ = id => document.getElementById(id);

function show(html) {
  $('view').innerHTML = html;
}

function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, 3200);
}

function confirmSheet(title, text, action, { danger = false } = {}) {
  return new Promise(resolve => {
    $('sheet-title').textContent = title;
    $('sheet-text').textContent = text;
    $('sheet-confirm').textContent = action;
    $('sheet-confirm').className = `button ${danger ? 'danger' : 'primary'}`;
    $('sheet').hidden = false;
    const done = value => {
      $('sheet').hidden = true;
      $('sheet-confirm').onclick = $('sheet-cancel').onclick = $('sheet').onclick = null;
      resolve(value);
    };
    $('sheet-confirm').onclick = () => done(true);
    $('sheet-cancel').onclick = () => done(false);
    $('sheet').onclick = event => { if (event.target === $('sheet')) done(false); };
  });
}

function schedule(fn, ms) {
  clearTimeout(app.timer);
  app.timer = setTimeout(fn, ms);
}

function handleError(error) {
  if (error.status === 401) {
    app.state = null;
    $('refresh').hidden = true;
    show(pairHTML(null));
    return;
  }
  if (error.status === 503 && error.body?.enabled === false) {
    $('refresh').hidden = true;
    show(messageHTML('Phone access is off', 'Turn it on in Switcher on your Mac, under Settings → Phone.'));
    return;
  }
  if (app.state) {
    toast(error.message || 'Switcher did not answer');
    return;
  }
  show(messageHTML('Switcher did not answer', error.message || 'Check that your Mac is awake and Tailscale is connected.'));
}

async function loadState() {
  try {
    app.state = await api('/api/state');
    $('refresh').hidden = false;
    if (!app.busy) show(stateHTML(app.state) + footerHTML(app.device));
    schedule(loadState, document.visibilityState === 'visible' ? 15000 : 60000);
  } catch (error) {
    handleError(error);
  }
}

async function poll() {
  try {
    const answer = await api('/api/pair');
    if (answer.status === 'approved') {
      app.csrf = answer.csrf;
      app.pairing = null;
      playMark($('top-mark'));
      toast('This phone is approved');
      await loadState();
      return;
    }
    if (answer.status === 'expired') {
      app.pairing = null;
      show(pairHTML(null));
      toast('The code expired; ask for a new one');
      return;
    }
    app.pairing = answer;
    show(pairHTML(answer));
    schedule(poll, 2000);
  } catch (error) {
    handleError(error);
  }
}

async function startPairing() {
  try {
    app.pairing = await api('/api/pair', { method: 'POST' });
    show(pairHTML(app.pairing));
    schedule(poll, 2000);
  } catch (error) {
    if (error.status === 429) toast(error.message);
    else handleError(error);
  }
}

async function boot() {
  $('top-mark').innerHTML = switcherMark({ tile: true, size: 34, id: 'top-mark-svg' });
  try {
    const session = await api('/api/session');
    app.mac = session.mac || '';
    app.device = session.device || '';
    $('top-mac').textContent = app.mac ? `on ${app.mac}` : 'on your Mac';
    if (session.paired) {
      app.csrf = session.csrf;
      await loadState();
    } else {
      show(pairHTML(null));
    }
  } catch (error) {
    handleError(error);
  }
}

async function act(button, run) {
  if (app.busy) return;
  app.busy = true;
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  try {
    await run();
  } catch (error) {
    if (error.status === 401 || error.status === 503) handleError(error);
    else toast(error.message || 'That did not work');
  } finally {
    app.busy = false;
    await loadState();
  }
}

function account(id) {
  return app.state?.accounts?.find(a => a.id === id);
}

if (typeof document !== 'undefined') {
  document.addEventListener('click', event => {
    const target = event.target.closest('button');
    if (!target) return;
    if (target.id === 'pair-start') startPairing();
    else if (target.id === 'retry') boot();
    else if (target.id === 'sign-out') {
      confirmSheet('Sign this phone out?', 'To use Switcher here again, you approve this phone on your Mac once more.', 'Sign out', { danger: true }).then(async ok => {
        if (!ok) return;
        try {
          await api('/api/logout', { method: 'POST' });
        } catch { /* signed out either way below */ }
        clearTimeout(app.timer);
        app.state = null;
        $('refresh').hidden = true;
        show(pairHTML(null));
      });
    }
    else if (target.id === 'refresh') {
      act(target, async () => {
        await api('/api/usage/refresh', { method: 'POST' });
        toast('Refreshing usage');
      }).finally(() => { target.disabled = false; target.removeAttribute('aria-busy'); });
    } else if (target.dataset.use) {
      const a = account(target.dataset.use);
      act(target, async () => {
        const answer = await api(`/api/accounts/${encodeURIComponent(target.dataset.use)}/use`, { method: 'POST' });
        playMark($('top-mark'));
        toast(answer.claude_code_switched ? `Claude and Claude Code now use ${a?.email || 'that account'}`
          : `${PROVIDERS[a?.provider] || 'Switcher'} now uses ${a?.email || 'that account'}`);
      });
    } else if (target.dataset.reset) {
      const a = account(target.dataset.reset);
      const credits = a?.reset_credits?.count || 0;
      confirmSheet('Use a banked reset?', `${a?.email || 'This account'} gets its usage back right away. You have ${credits} banked reset${credits === 1 ? '' : 's'}; a used reset cannot be given back.`, 'Use reset')
        .then(ok => {
          if (!ok) return;
          act(target, async () => {
            const answer = await api(`/api/accounts/${encodeURIComponent(a.id)}/reset`, { method: 'POST', body: { credit_id: a.reset_credits.next_id } });
            if (answer.outcome === 'reset') playMark($('top-mark'));
            toast(OUTCOMES[answer.outcome] || `The provider answered ${answer.outcome}`);
          });
        });
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && app.state) loadState();
  });
  boot();
}
