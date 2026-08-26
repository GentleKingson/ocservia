export interface OperationDetailState<T> {
  selected?: T;
  error: string;
}

export function startOperationDetail<T>(): OperationDetailState<T> {
  return { error: "" };
}

export function completeOperationDetail<T>(
  selected: T,
): OperationDetailState<T> {
  return { selected, error: "" };
}

export function failOperationDetail<T>(error: string): OperationDetailState<T> {
  return { error };
}
