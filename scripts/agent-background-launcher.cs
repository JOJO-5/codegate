// Compile with .NET Framework csc /target:winexe. The outermost process has no
// console subsystem; the Agent is created without a console before any Go code
// or update fallback diagnostic runs. Keep the job handle private to this host.
using System;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using Microsoft.Win32.SafeHandles;

internal static class AgentBackgroundLauncher {
    private static readonly object LogLock = new object();
    private static string LogPath;
    private const uint CREATE_SUSPENDED = 4, CREATE_NO_WINDOW = 0x08000000, CREATE_UNICODE_ENVIRONMENT = 0x400;
    [StructLayout(LayoutKind.Sequential)] private struct Security { public int Length; public IntPtr Descriptor; [MarshalAs(UnmanagedType.Bool)] public bool Inherit; }
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] private struct Startup {
        public int cb; public string Reserved, Desktop, Title;
        public uint X,Y,XSize,YSize,XChars,YChars,Fill,Flags;
        public ushort Show,Reserved2; public IntPtr ReservedPtr,Input,Output,Error;
    }
    [StructLayout(LayoutKind.Sequential)] private struct ProcessInfo { public IntPtr Process,Thread; public uint PID,TID; }
    [StructLayout(LayoutKind.Sequential)] private struct BasicLimits { public long ProcessTime,JobTime; public uint Flags; public UIntPtr MinWorking,MaxWorking; public uint Active; public UIntPtr Affinity; public uint Priority,Scheduling; }
    [StructLayout(LayoutKind.Sequential)] private struct IOCounters { public ulong ReadOps,WriteOps,OtherOps,ReadBytes,WriteBytes,OtherBytes; }
    [StructLayout(LayoutKind.Sequential)] private struct Limits { public BasicLimits Basic; public IOCounters IO; public UIntPtr ProcessMemory,JobMemory,PeakProcess,PeakJob; }
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] private static extern IntPtr CreateJobObject(IntPtr attributes,string name);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool SetInformationJobObject(IntPtr job,int kind,ref Limits limits,uint size);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool AssignProcessToJobObject(IntPtr job,IntPtr process);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool CreatePipe(out IntPtr read,out IntPtr write,ref Security attributes,uint size);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool SetHandleInformation(IntPtr handle,uint mask,uint flags);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] private static extern IntPtr CreateFile(string name,uint access,uint share,ref Security attributes,uint creation,uint flags,IntPtr template);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] private static extern bool CreateProcess(string application,StringBuilder command,IntPtr processAttributes,IntPtr threadAttributes,bool inherit,uint flags,IntPtr environment,string directory,ref Startup startup,out ProcessInfo process);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern uint ResumeThread(IntPtr thread);
    [DllImport("kernel32.dll")] private static extern bool TerminateProcess(IntPtr process,uint code);
    [DllImport("kernel32.dll")] private static extern uint WaitForSingleObject(IntPtr handle,uint timeout);
    [DllImport("kernel32.dll", SetLastError=true)] private static extern bool GetExitCodeProcess(IntPtr process,out uint code);
    [DllImport("kernel32.dll")] private static extern bool CloseHandle(IntPtr handle);
    private static void Close(ref IntPtr handle) { if(handle!=IntPtr.Zero && handle!=new IntPtr(-1)) CloseHandle(handle); handle=IntPtr.Zero; }
    private static void Check(bool ok) { if(!ok) throw new Win32Exception(Marshal.GetLastWin32Error()); }
    private static void SafeLogFile(string path) { if(File.Exists(path) && (File.GetAttributes(path)&(FileAttributes.ReparsePoint|FileAttributes.Directory))!=0) throw new IOException("Log path is not a regular file"); }
    private static void Log(string text) {
        lock(LogLock) { try {
            SafeLogFile(LogPath); SafeLogFile(LogPath+".1");
            if(File.Exists(LogPath) && new FileInfo(LogPath).Length>=1024*1024) {
                if(File.Exists(LogPath+".1")) File.Delete(LogPath+".1");
                File.Move(LogPath,LogPath+".1");
            }
            File.AppendAllText(LogPath,DateTimeOffset.Now.ToString("o")+" "+text+Environment.NewLine,new UTF8Encoding(false));
        } catch { /* Logging failures cannot expose a console or secrets. */ } }
    }
    private static Thread Reader(IntPtr handle) {
        Thread thread=new Thread(delegate() {
            try { using(var file=new FileStream(new SafeFileHandle(handle,true),FileAccess.Read))
                  using(var reader=new StreamReader(file,Encoding.UTF8,true)) {
                char[] chunk=new char[1024]; var line=new StringBuilder(); int count;
                while((count=reader.Read(chunk,0,chunk.Length))>0) for(int i=0;i<count;i++) {
                    if(chunk[i]=='\n') {Log(line.ToString().TrimEnd('\r'));line.Length=0;}
                    else if(line.Length<4096) line.Append(chunk[i]);
                }
                if(line.Length>0) Log(line.ToString());
            }} catch { Log("Agent log stream closed"); }
        });thread.IsBackground=true;thread.Start();return thread;
    }
    [STAThread] private static int Main(string[] args) {
        if(args.Length!=2 || args[0]!="-config" || !Path.IsPathRooted(args[1]) || !File.Exists(args[1]) || args[1].IndexOf('"')>=0) return 2;
        string directory=AppDomain.CurrentDomain.BaseDirectory;
        string agent=Path.Combine(directory,"codegate-agent.exe");
        IntPtr job=IntPtr.Zero,outputRead=IntPtr.Zero,outputWrite=IntPtr.Zero,errorRead=IntPtr.Zero,errorWrite=IntPtr.Zero,input=IntPtr.Zero;
        ProcessInfo child=new ProcessInfo(); Thread outputThread=null,errorThread=null;
        try {
            string logs=Path.GetFullPath(Path.Combine(directory,"..","logs"));Directory.CreateDirectory(logs);
            if((File.GetAttributes(logs)&FileAttributes.ReparsePoint)!=0) return 3;
            LogPath=Path.Combine(logs,"agent-background.log");
            if(!File.Exists(agent)) {Log("Agent executable missing");return 4;}
            job=CreateJobObject(IntPtr.Zero,null);Check(job!=IntPtr.Zero);
            Limits limits=new Limits();limits.Basic.Flags=0x2000; // KILL_ON_JOB_CLOSE
            Check(SetInformationJobObject(job,9,ref limits,(uint)Marshal.SizeOf(typeof(Limits))));
            Security security=new Security();security.Length=Marshal.SizeOf(typeof(Security));security.Inherit=true;
            Check(CreatePipe(out outputRead,out outputWrite,ref security,0));Check(SetHandleInformation(outputRead,1,0));
            Check(CreatePipe(out errorRead,out errorWrite,ref security,0));Check(SetHandleInformation(errorRead,1,0));
            input=CreateFile("NUL",0x80000000,3,ref security,3,0,IntPtr.Zero);Check(input!=new IntPtr(-1));
            Startup startup=new Startup();startup.cb=Marshal.SizeOf(typeof(Startup));startup.Flags=0x100;startup.Input=input;startup.Output=outputWrite;startup.Error=errorWrite;
            string command="\""+agent+"\" supervise -config \""+args[1]+"\"";
            Check(CreateProcess(agent,new StringBuilder(command),IntPtr.Zero,IntPtr.Zero,true,CREATE_SUSPENDED|CREATE_NO_WINDOW|CREATE_UNICODE_ENVIRONMENT,IntPtr.Zero,directory,ref startup,out child));
            // Assign while suspended: no descendant can escape ownership even if
            // Task Scheduler stops this host immediately after process creation.
            Check(AssignProcessToJobObject(job,child.Process));
            Check(ResumeThread(child.Thread)!=0xffffffff);Close(ref child.Thread);
            Close(ref outputWrite);Close(ref errorWrite);Close(ref input);
            outputThread=Reader(outputRead);outputRead=IntPtr.Zero;errorThread=Reader(errorRead);errorRead=IntPtr.Zero;
            Check(WaitForSingleObject(child.Process,0xffffffff)==0);
            uint exitCode;Check(GetExitCodeProcess(child.Process,out exitCode));return unchecked((int)exitCode);
        } catch(Exception error) {
            if(LogPath!=null) Log("Background Agent failed: "+error.GetType().Name+" "+error.Message);
            return 1;
        } finally {
            if(child.Process!=IntPtr.Zero) TerminateProcess(child.Process,1);
            Close(ref job);Close(ref child.Thread);Close(ref child.Process);
            Close(ref outputWrite);Close(ref errorWrite);Close(ref input);Close(ref outputRead);Close(ref errorRead);
            if(outputThread!=null) outputThread.Join(2000);if(errorThread!=null) errorThread.Join(2000);
        }
    }
}
