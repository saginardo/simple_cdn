import { useEffect, useState, type Dispatch, type SetStateAction } from "react";

type Validator<T> = (value: unknown) => value is T;

function readStored<T>(key: string, fallback: T, validate: Validator<T>) {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return fallback;
    const value: unknown = JSON.parse(raw);
    return validate(value) ? value : fallback;
  } catch {
    return fallback;
  }
}

export function usePersistentState<T>(
  key: string,
  fallback: T,
  validate: Validator<T>,
): [T, Dispatch<SetStateAction<T>>] {
  const [value, setValue] = useState<T>(() =>
    readStored(key, fallback, validate),
  );

  useEffect(() => {
    try {
      window.localStorage.setItem(key, JSON.stringify(value));
    } catch {
      // A disabled or full storage area should not break page controls.
    }
  }, [key, value]);

  return [value, setValue];
}

export function usePersistentEnum<const T extends string>(
  key: string,
  values: readonly T[],
  fallback: T,
) {
  return usePersistentState<T>(
    key,
    fallback,
    (value): value is T =>
      typeof value === "string" && values.includes(value as T),
  );
}
