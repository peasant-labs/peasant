export interface ShareFooterAction {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  title?: string;
}

export interface ShareFooterActions {
  primary: ShareFooterAction;
  secondary?: ShareFooterAction;
}

export type SetShareFooterActions = (actions: ShareFooterActions | null) => void;
