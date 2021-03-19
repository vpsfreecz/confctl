module ConfCtl
  class MachineControl
    # @return [Deployment]
    attr_reader :deployment

    # @param deployment [Deployment]
    def initialize(deployment)
      @deployment = deployment
    end

    # @return [Process::Status]
    def execute(*command)
      system_exec(*command)
    end

    protected
    # @return [Process::Status]
    def system_exec(*command)
      pid =
        if deployment.localhost?
          Process.spawn(*command)
        else
          Process.spawn('ssh', "root@#{deployment.target_host}", *command)
        end

      Process.wait(pid)
      $?
    end
  end
end
