// Known-city catalog mirrored from backend/internal/segment/cities.go (the
// 81 provinces of Türkiye, licence-plate order). Normalized segment rules
// store the folded key (e.g. "istanbul"); the UI shows the Turkish display
// name. A key that is not in this catalog is shown verbatim, never guessed.

const CITY_NAMES: readonly string[] = [
  'Adana', 'Adıyaman', 'Afyonkarahisar', 'Ağrı', 'Amasya', 'Ankara', 'Antalya', 'Artvin',
  'Aydın', 'Balıkesir', 'Bilecik', 'Bingöl', 'Bitlis', 'Bolu', 'Burdur', 'Bursa',
  'Çanakkale', 'Çankırı', 'Çorum', 'Denizli', 'Diyarbakır', 'Edirne', 'Elazığ', 'Erzincan',
  'Erzurum', 'Eskişehir', 'Gaziantep', 'Giresun', 'Gümüşhane', 'Hakkari', 'Hatay', 'Isparta',
  'Mersin', 'İstanbul', 'İzmir', 'Kars', 'Kastamonu', 'Kayseri', 'Kırklareli', 'Kırşehir',
  'Kocaeli', 'Konya', 'Kütahya', 'Malatya', 'Manisa', 'Kahramanmaraş', 'Mardin', 'Muğla',
  'Muş', 'Nevşehir', 'Niğde', 'Ordu', 'Rize', 'Sakarya', 'Samsun', 'Siirt',
  'Sinop', 'Sivas', 'Tekirdağ', 'Tokat', 'Trabzon', 'Tunceli', 'Şanlıurfa', 'Uşak',
  'Van', 'Yozgat', 'Zonguldak', 'Aksaray', 'Bayburt', 'Karaman', 'Kırıkkale', 'Batman',
  'Şırnak', 'Bartın', 'Ardahan', 'Iğdır', 'Yalova', 'Karabük', 'Kilis', 'Osmaniye',
  'Düzce',
]

/**
 * Turkish-aware folding identical to segment.FoldKey: NFC, Turkish lower-case
 * (I→ı, İ→i), fold ı ş ğ ü ö ç to ASCII, trim and collapse whitespace.
 */
export function foldCityKey(s: string): string {
  const lowered = s
    .normalize('NFC')
    .replace(/I/g, 'ı')
    .replace(/İ/g, 'i')
    .toLowerCase()
  return lowered
    .replace(/ı/g, 'i')
    .replace(/ş/g, 's')
    .replace(/ğ/g, 'g')
    .replace(/ü/g, 'u')
    .replace(/ö/g, 'o')
    .replace(/ç/g, 'c')
    .split(/\s+/)
    .filter(Boolean)
    .join(' ')
}

const BY_KEY: ReadonlyMap<string, string> = new Map(CITY_NAMES.map((n) => [foldCityKey(n), n]))

/** Display name for a normalized city key, or null when the key is unknown. */
export function cityName(key: string): string | null {
  return BY_KEY.get(foldCityKey(key)) ?? null
}

export const CITY_COUNT = CITY_NAMES.length
